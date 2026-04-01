package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	clierr "github.com/ggonzalez94/defi-cli/internal/errors"
	"github.com/ggonzalez94/defi-cli/internal/execution"
	"github.com/ggonzalez94/defi-cli/internal/id"
)

const tellerAPIBase = "https://delta-neutral-api.teller.org"

// TellerLendRequest describes a borrow or repay action on Teller Protocol.
type TellerLendRequest struct {
	Verb            AaveLendVerb // borrow or repay
	Chain           id.Chain
	Asset           id.Asset // the borrow (principal) token
	AmountBaseUnits string
	Sender          string
	Recipient       string
	Simulate        bool
	RPCURL          string

	// Borrow-specific fields
	PoolAddress      string // Teller pool address (required for borrow)
	CollateralToken  string // collateral token address (required for borrow)
	CollateralAmount string // collateral amount in base units (required for borrow)
	LoanDuration     int    // loan duration in seconds (optional, defaults to 30 days)

	// Repay-specific fields
	LoanID int // bid ID of the loan to repay (required for repay)
}

type tellerTx struct {
	To           string `json:"to"`
	Data         string `json:"data"`
	FunctionName string `json:"functionName"`
	Description  string `json:"description"`
	Value        string `json:"value"`
}

type tellerTxResponse struct {
	Transactions []tellerTx     `json:"transactions"`
	Summary      map[string]any `json:"summary"`
}

// BuildTellerLendAction constructs an execution.Action for a Teller borrow or repay.
// Teller's API returns pre-built transaction calldata, so we call the API during planning.
func BuildTellerLendAction(ctx context.Context, req TellerLendRequest) (execution.Action, error) {
	verb := strings.ToLower(strings.TrimSpace(string(req.Verb)))
	sender := strings.TrimSpace(req.Sender)
	if !common.IsHexAddress(sender) {
		return execution.Action{}, clierr.New(clierr.CodeUsage, "teller lend action requires sender address")
	}
	recipient := strings.TrimSpace(req.Recipient)
	if recipient == "" {
		recipient = sender
	}
	if !common.IsHexAddress(recipient) {
		return execution.Action{}, clierr.New(clierr.CodeUsage, "invalid recipient address")
	}

	action := execution.NewAction(execution.NewActionID(), "lend_"+verb, req.Chain.CAIP2, execution.Constraints{Simulate: req.Simulate})
	action.Provider = "teller"
	action.FromAddress = common.HexToAddress(sender).Hex()
	action.ToAddress = common.HexToAddress(recipient).Hex()
	action.InputAmount = strings.TrimSpace(req.AmountBaseUnits)
	action.Metadata = map[string]any{
		"protocol":       "teller",
		"asset_id":       req.Asset.AssetID,
		"lending_action": verb,
	}

	switch verb {
	case string(AaveVerbBorrow):
		poolAddress := strings.TrimSpace(req.PoolAddress)
		if !common.IsHexAddress(poolAddress) {
			return execution.Action{}, clierr.New(clierr.CodeUsage, "--pool-address is required for teller borrow (the Teller pool address)")
		}
		collateralToken := strings.TrimSpace(req.CollateralToken)
		if !common.IsHexAddress(collateralToken) {
			return execution.Action{}, clierr.New(clierr.CodeUsage, "--collateral-token is required for teller borrow")
		}
		collateralAmount := strings.TrimSpace(req.CollateralAmount)
		if collateralAmount == "" || collateralAmount == "0" {
			return execution.Action{}, clierr.New(clierr.CodeUsage, "--collateral-amount is required for teller borrow")
		}

		action.Metadata["pool_address"] = poolAddress
		action.Metadata["collateral_token"] = collateralToken
		action.Metadata["collateral_amount"] = collateralAmount

		txResp, err := tellerFetchBorrowTx(ctx, sender, collateralToken, req.Chain.EVMChainID, poolAddress, collateralAmount, req.AmountBaseUnits, req.LoanDuration)
		if err != nil {
			return execution.Action{}, err
		}

		for i, tx := range txResp.Transactions {
			stepType := execution.StepTypeLend
			stepID := fmt.Sprintf("teller-borrow-%d", i)
			if strings.Contains(strings.ToLower(tx.FunctionName), "approve") {
				stepType = execution.StepTypeApproval
				stepID = fmt.Sprintf("teller-approve-%d", i)
			}
			action.Steps = append(action.Steps, execution.ActionStep{
				StepID:      stepID,
				Type:        stepType,
				Status:      execution.StepStatusPending,
				ChainID:     req.Chain.CAIP2,
				RPCURL:      req.RPCURL,
				Description: tx.Description,
				Target:      tx.To,
				Data:        tx.Data,
				Value:       tx.Value,
			})
		}

	case string(AaveVerbRepay):
		if req.LoanID <= 0 {
			return execution.Action{}, clierr.New(clierr.CodeUsage, "--loan-id is required for teller repay")
		}

		action.Metadata["loan_id"] = req.LoanID

		repayAmount := strings.TrimSpace(req.AmountBaseUnits)
		txResp, err := tellerFetchRepayTx(ctx, req.LoanID, req.Chain.EVMChainID, sender, repayAmount)
		if err != nil {
			return execution.Action{}, err
		}

		for i, tx := range txResp.Transactions {
			stepType := execution.StepTypeLend
			stepID := fmt.Sprintf("teller-repay-%d", i)
			if strings.Contains(strings.ToLower(tx.FunctionName), "approve") {
				stepType = execution.StepTypeApproval
				stepID = fmt.Sprintf("teller-repay-approve-%d", i)
			}
			action.Steps = append(action.Steps, execution.ActionStep{
				StepID:      stepID,
				Type:        stepType,
				Status:      execution.StepStatusPending,
				ChainID:     req.Chain.CAIP2,
				RPCURL:      req.RPCURL,
				Description: tx.Description,
				Target:      tx.To,
				Data:        tx.Data,
				Value:       tx.Value,
			})
		}

	default:
		return execution.Action{}, clierr.New(clierr.CodeUsage, fmt.Sprintf("teller only supports borrow and repay actions, got %q", verb))
	}

	return action, nil
}

// tellerFetchBorrowTx calls the Teller /borrow-tx API to get pre-built transaction calldata.
func tellerFetchBorrowTx(ctx context.Context, wallet, collateralToken string, chainID int64, poolAddress, collateralAmount, principalAmount string, loanDuration int) (*tellerTxResponse, error) {
	u, _ := url.Parse(tellerAPIBase + "/borrow-tx")
	q := u.Query()
	q.Set("walletAddress", wallet)
	q.Set("collateralTokenAddress", collateralToken)
	q.Set("chainId", strconv.FormatInt(chainID, 10))
	q.Set("poolAddress", poolAddress)
	q.Set("collateralAmount", collateralAmount)
	q.Set("principalAmount", principalAmount)
	if loanDuration > 0 {
		q.Set("loanDuration", strconv.Itoa(loanDuration))
	}
	u.RawQuery = q.Encode()

	var resp tellerTxResponse
	if err := tellerGet(ctx, u.String(), &resp); err != nil {
		return nil, clierr.Wrap(clierr.CodeUnavailable, "fetch teller borrow transactions", err)
	}
	return &resp, nil
}

// tellerFetchRepayTx calls the Teller /loans/repay-tx API to get pre-built repayment calldata.
func tellerFetchRepayTx(ctx context.Context, bidID int, chainID int64, wallet, amount string) (*tellerTxResponse, error) {
	u, _ := url.Parse(tellerAPIBase + "/loans/repay-tx")
	q := u.Query()
	q.Set("bidId", strconv.Itoa(bidID))
	q.Set("chainId", strconv.FormatInt(chainID, 10))
	q.Set("walletAddress", wallet)
	if amount != "" {
		q.Set("amount", amount)
	}
	u.RawQuery = q.Encode()

	var resp tellerTxResponse
	if err := tellerGet(ctx, u.String(), &resp); err != nil {
		return nil, clierr.Wrap(clierr.CodeUnavailable, "fetch teller repay transactions", err)
	}
	return &resp, nil
}

func tellerGet(ctx context.Context, rawURL string, out any) error {
	httpClient := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "defi-cli/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("teller API returned status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
