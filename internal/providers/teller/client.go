package teller

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	clierr "github.com/ggonzalez94/defi-cli/internal/errors"
	"github.com/ggonzalez94/defi-cli/internal/httpx"
	"github.com/ggonzalez94/defi-cli/internal/id"
	"github.com/ggonzalez94/defi-cli/internal/model"
	"github.com/ggonzalez94/defi-cli/internal/providers"
)

const defaultBaseURL = "https://delta-neutral-api.teller.org"

// Client implements the LendingProvider and LendingPositionsProvider interfaces
// for the Teller Protocol. Teller is a commitment-based borrowing protocol with
// no margin-call liquidations.
type Client struct {
	http    *httpx.Client
	baseURL string
	now     func() time.Time
}

func New(httpClient *httpx.Client) *Client {
	return &Client{http: httpClient, baseURL: defaultBaseURL, now: time.Now}
}

func (c *Client) Info() model.ProviderInfo {
	return model.ProviderInfo{
		Name:        "teller",
		Type:        "lending",
		RequiresKey: false,
		Capabilities: []string{
			"lend.markets",
			"lend.rates",
			"lend.positions",
			"lend.plan",
			"lend.execute",
		},
	}
}

// --- API response types ---

type borrowPool struct {
	ChainID                int    `json:"chainId"`
	PoolAddress            string `json:"pool_address"`
	CollateralTokenAddress string `json:"collateral_token_address"`
	CollateralTokenSymbol  string `json:"collateral_token_symbol"`
	BorrowTokenAddress     string `json:"borrow_token_address"`
	BorrowTokenSymbol      string `json:"borrow_token_symbol"`
	Active                 bool   `json:"active"`
	IsV2                   *bool  `json:"isV2,omitempty"`
	Enrichment             *poolEnrichment `json:"enrichment,omitempty"`
	EnrichmentError        string          `json:"enrichmentError,omitempty"`
}

type poolEnrichment struct {
	MarketID             *int     `json:"marketId,omitempty"`
	MarketplaceFeePct    *float64 `json:"marketplaceFeePct,omitempty"`
	PaymentCycleDuration *int     `json:"paymentCycleDuration,omitempty"`
	CollateralRatioBps   *int     `json:"collateralRatioBps,omitempty"`
	CollateralRatioPct   *float64 `json:"collateralRatioPct,omitempty"`
	PrincipalToken       *string  `json:"principalToken,omitempty"`
	PrincipalTokenDecimals *int   `json:"principalTokenDecimals,omitempty"`
	MinInterestRateBps   *int     `json:"minInterestRateBps,omitempty"`
	MinInterestRatePct   *float64 `json:"minInterestRatePct,omitempty"`
	PrincipalAvailableRaw *string `json:"principalAvailableRaw,omitempty"`
	PrincipalAvailable    *float64 `json:"principalAvailable,omitempty"`
	PrincipalAvailableUsd *float64 `json:"principalAvailableUsd,omitempty"`
}

type poolsResponse struct {
	UpdatedAt int          `json:"updated_at"`
	TTLMS     int          `json:"ttl_ms"`
	Count     int          `json:"count"`
	Results   []borrowPool `json:"results"`
}

type loan struct {
	BidID              string  `json:"bidId"`
	BorrowerAddress    string  `json:"borrowerAddress"`
	LenderAddress      string  `json:"lenderAddress"`
	LendingTokenAddress string `json:"lendingTokenAddress"`
	Principal          string  `json:"principal"`
	Status             string  `json:"status"`
	APR                string  `json:"apr,omitempty"`
	LoanDuration       string  `json:"loanDuration,omitempty"`
	PaymentCycle       string  `json:"paymentCycle,omitempty"`
	AcceptedTimestamp  string  `json:"acceptedTimestamp,omitempty"`
	NextDueDate        string  `json:"nextDueDate,omitempty"`
}

type loansResponse struct {
	WalletAddress string `json:"walletAddress"`
	ChainID       int    `json:"chainId"`
	Count         int    `json:"count"`
	Loans         []loan `json:"loans"`
}

// --- LendingProvider interface ---

func (c *Client) LendMarkets(ctx context.Context, provider string, chain id.Chain, asset id.Asset) ([]model.LendMarket, error) {
	if !strings.EqualFold(strings.TrimSpace(provider), "teller") {
		return nil, clierr.New(clierr.CodeUnsupported, "teller adapter supports only provider=teller")
	}
	if !chain.IsEVM() {
		return nil, clierr.New(clierr.CodeUnsupported, "teller only supports EVM chains")
	}

	pools, err := c.fetchPools(ctx, chain.EVMChainID, asset.Address)
	if err != nil {
		return nil, err
	}

	fetchedAt := c.now().UTC().Format(time.RFC3339)
	markets := make([]model.LendMarket, 0, len(pools))
	for _, pool := range pools {
		if !pool.Active {
			continue
		}
		if !matchesAsset(pool, asset) {
			continue
		}

		var borrowAPY, tvl, liquidity float64
		if e := pool.Enrichment; e != nil {
			if e.MinInterestRatePct != nil {
				borrowAPY = *e.MinInterestRatePct / 100.0
			}
			if e.PrincipalAvailableUsd != nil {
				tvl = *e.PrincipalAvailableUsd
				liquidity = *e.PrincipalAvailableUsd
			}
		}

		// Use borrow token as asset ID for the market
		assetID := canonicalAssetID(chain.CAIP2, pool.BorrowTokenAddress)

		markets = append(markets, model.LendMarket{
			Protocol:             "teller",
			Provider:             "teller",
			ChainID:              chain.CAIP2,
			AssetID:              assetID,
			ProviderNativeID:     pool.PoolAddress,
			ProviderNativeIDKind: model.NativeIDKindPoolID,
			SupplyAPY:            0, // Teller is borrow-only; no supply side
			BorrowAPY:            borrowAPY,
			TVLUSD:               tvl,
			LiquidityUSD:         liquidity,
			FetchedAt:            fetchedAt,
		})
	}
	return markets, nil
}

func (c *Client) LendRates(ctx context.Context, provider string, chain id.Chain, asset id.Asset) ([]model.LendRate, error) {
	if !strings.EqualFold(strings.TrimSpace(provider), "teller") {
		return nil, clierr.New(clierr.CodeUnsupported, "teller adapter supports only provider=teller")
	}
	if !chain.IsEVM() {
		return nil, clierr.New(clierr.CodeUnsupported, "teller only supports EVM chains")
	}

	pools, err := c.fetchPools(ctx, chain.EVMChainID, asset.Address)
	if err != nil {
		return nil, err
	}

	fetchedAt := c.now().UTC().Format(time.RFC3339)
	rates := make([]model.LendRate, 0, len(pools))
	for _, pool := range pools {
		if !pool.Active {
			continue
		}
		if !matchesAsset(pool, asset) {
			continue
		}

		var borrowAPY, utilization float64
		if e := pool.Enrichment; e != nil {
			if e.MinInterestRatePct != nil {
				borrowAPY = *e.MinInterestRatePct / 100.0
			}
			// Teller doesn't expose utilization directly; leave as 0
		}

		assetID := canonicalAssetID(chain.CAIP2, pool.BorrowTokenAddress)

		rates = append(rates, model.LendRate{
			Protocol:             "teller",
			Provider:             "teller",
			ChainID:              chain.CAIP2,
			AssetID:              assetID,
			ProviderNativeID:     pool.PoolAddress,
			ProviderNativeIDKind: model.NativeIDKindPoolID,
			SupplyAPY:            0,
			BorrowAPY:            borrowAPY,
			Utilization:          utilization,
			FetchedAt:            fetchedAt,
		})
	}
	return rates, nil
}

// --- LendingPositionsProvider interface ---

func (c *Client) LendPositions(ctx context.Context, req providers.LendPositionsRequest) ([]model.LendPosition, error) {
	if !req.Chain.IsEVM() {
		return nil, clierr.New(clierr.CodeUnsupported, "teller only supports EVM chains")
	}

	loans, err := c.fetchLoans(ctx, req.Account, req.Chain.EVMChainID)
	if err != nil {
		return nil, err
	}

	fetchedAt := c.now().UTC().Format(time.RFC3339)
	positions := make([]model.LendPosition, 0, len(loans))
	for _, ln := range loans {
		if ln.Status != "active" {
			continue
		}
		posType := "borrow"
		if req.PositionType != providers.LendPositionTypeAll && string(req.PositionType) != posType {
			continue
		}

		assetID := canonicalAssetID(req.Chain.CAIP2, ln.LendingTokenAddress)
		if req.Asset.Address != "" && !strings.EqualFold(req.Asset.Address, ln.LendingTokenAddress) {
			continue
		}

		var apy float64
		if ln.APR != "" {
			if v, err := strconv.ParseFloat(ln.APR, 64); err == nil {
				apy = v / 100.0
			}
		}

		// Parse principal into decimal representation
		principalBase := strings.TrimSpace(ln.Principal)
		decimals := 18 // default; lending token decimals not always known here
		tok, found := id.LookupByAddress(req.Chain.CAIP2, ln.LendingTokenAddress)
		if found && tok.Decimals > 0 {
			decimals = tok.Decimals
		}
		amountDecimal := baseUnitsToDecimal(principalBase, decimals)

		positions = append(positions, model.LendPosition{
			Protocol:             "teller",
			Provider:             "teller",
			ChainID:              req.Chain.CAIP2,
			AccountAddress:       req.Account,
			PositionType:         posType,
			AssetID:              assetID,
			ProviderNativeID:     ln.BidID,
			ProviderNativeIDKind: "loan_id",
			Amount: model.AmountInfo{
				AmountBaseUnits: principalBase,
				AmountDecimal:   amountDecimal,
				Decimals:        decimals,
			},
			APY:       apy,
			FetchedAt: fetchedAt,
		})

		if req.Limit > 0 && len(positions) >= req.Limit {
			break
		}
	}
	return positions, nil
}

// --- API helpers ---

func (c *Client) fetchPools(ctx context.Context, chainID int64, tokenAddress string) ([]borrowPool, error) {
	u, _ := url.Parse(c.baseURL + "/borrow/general")
	q := u.Query()
	q.Set("chainId", strconv.FormatInt(chainID, 10))
	if tokenAddress != "" {
		q.Set("borrow_token_address", tokenAddress)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, clierr.Wrap(clierr.CodeInternal, "build teller pools request", err)
	}

	var resp poolsResponse
	if _, err := c.http.DoJSON(ctx, req, &resp); err != nil {
		return nil, clierr.Wrap(clierr.CodeUnavailable, "fetch teller pools", err)
	}
	return resp.Results, nil
}

func (c *Client) fetchLoans(ctx context.Context, wallet string, chainID int64) ([]loan, error) {
	u, _ := url.Parse(c.baseURL + "/loans/get-all")
	q := u.Query()
	q.Set("walletAddress", wallet)
	q.Set("chainId", strconv.FormatInt(chainID, 10))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, clierr.Wrap(clierr.CodeInternal, "build teller loans request", err)
	}

	var resp loansResponse
	if _, err := c.http.DoJSON(ctx, req, &resp); err != nil {
		return nil, clierr.Wrap(clierr.CodeUnavailable, "fetch teller loans", err)
	}
	return resp.Loans, nil
}

// --- helpers ---

func matchesAsset(pool borrowPool, asset id.Asset) bool {
	if asset.Address == "" && asset.Symbol == "" {
		return true // no filter
	}
	if asset.Address != "" {
		return strings.EqualFold(pool.BorrowTokenAddress, asset.Address) ||
			strings.EqualFold(pool.CollateralTokenAddress, asset.Address)
	}
	if asset.Symbol != "" {
		sym := strings.ToUpper(asset.Symbol)
		return strings.ToUpper(pool.BorrowTokenSymbol) == sym ||
			strings.ToUpper(pool.CollateralTokenSymbol) == sym
	}
	return false
}

func canonicalAssetID(chainCAIP2, address string) string {
	addr := strings.ToLower(strings.TrimSpace(address))
	return fmt.Sprintf("%s/erc20:%s", chainCAIP2, addr)
}

func baseUnitsToDecimal(baseUnits string, decimals int) string {
	baseUnits = strings.TrimSpace(baseUnits)
	if baseUnits == "" || baseUnits == "0" {
		return "0"
	}
	// Simple integer-based decimal conversion
	if v, err := strconv.ParseFloat(baseUnits, 64); err == nil {
		dec := v / math.Pow10(decimals)
		return strconv.FormatFloat(dec, 'f', -1, 64)
	}
	return baseUnits
}
