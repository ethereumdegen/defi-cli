package app

import (
	"strings"
	"time"

	clierr "github.com/ggonzalez94/defi-cli/internal/errors"
	"github.com/ggonzalez94/defi-cli/internal/execution"
	"github.com/ggonzalez94/defi-cli/internal/execution/actionbuilder"
	execsigner "github.com/ggonzalez94/defi-cli/internal/execution/signer"
	"github.com/ggonzalez94/defi-cli/internal/id"
	"github.com/ggonzalez94/defi-cli/internal/model"
	"github.com/spf13/cobra"
)

func (s *runtimeState) newTransferCommand() *cobra.Command {
	root := &cobra.Command{Use: "transfer", Short: "ERC-20 transfer execution commands"}

	type transferArgs struct {
		chainArg      string
		assetArg      string
		amountBase    string
		amountDecimal string
		fromAddress   string
		recipient     string
		simulate      bool
		rpcURL        string
	}
	buildAction := func(args transferArgs) (execution.Action, error) {
		chain, asset, err := parseChainAsset(args.chainArg, args.assetArg)
		if err != nil {
			return execution.Action{}, err
		}
		decimals := asset.Decimals
		if decimals <= 0 {
			decimals = 18
		}
		base, _, err := id.NormalizeAmount(args.amountBase, args.amountDecimal, decimals)
		if err != nil {
			return execution.Action{}, err
		}
		return s.actionBuilderRegistry().BuildTransferAction(actionbuilder.TransferRequest{
			Chain:           chain,
			Asset:           asset,
			AmountBaseUnits: base,
			Sender:          args.fromAddress,
			Recipient:       args.recipient,
			Simulate:        args.simulate,
			RPCURL:          args.rpcURL,
		})
	}

	var plan transferArgs
	planCmd := &cobra.Command{
		Use:   "plan",
		Short: "Create and persist an ERC-20 transfer action plan",
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := time.Now()
			action, err := buildAction(plan)
			status := []model.ProviderStatus{{Name: "native", Status: statusFromErr(err), LatencyMS: time.Since(start).Milliseconds()}}
			if err != nil {
				s.captureCommandDiagnostics(nil, status, false)
				return err
			}
			if err := s.ensureActionStore(); err != nil {
				return err
			}
			if err := s.actionStore.Save(action); err != nil {
				return clierr.Wrap(clierr.CodeInternal, "persist planned action", err)
			}
			s.captureCommandDiagnostics(nil, status, false)
			return s.emitSuccess(trimRootPath(cmd.CommandPath()), action, nil, cacheMetaBypass(), status, false)
		},
	}
	planCmd.Flags().StringVar(&plan.chainArg, "chain", "", "Chain identifier")
	planCmd.Flags().StringVar(&plan.assetArg, "asset", "", "Asset symbol/address/CAIP-19")
	planCmd.Flags().StringVar(&plan.amountBase, "amount", "", "Amount in base units")
	planCmd.Flags().StringVar(&plan.amountDecimal, "amount-decimal", "", "Amount in decimal units")
	planCmd.Flags().StringVar(&plan.fromAddress, "from-address", "", "Sender EOA address")
	planCmd.Flags().StringVar(&plan.recipient, "recipient", "", "Recipient EOA address")
	planCmd.Flags().BoolVar(&plan.simulate, "simulate", true, "Include simulation checks during execution")
	planCmd.Flags().StringVar(&plan.rpcURL, "rpc-url", "", "RPC URL override for the selected chain")
	_ = planCmd.MarkFlagRequired("chain")
	_ = planCmd.MarkFlagRequired("asset")
	_ = planCmd.MarkFlagRequired("from-address")
	_ = planCmd.MarkFlagRequired("recipient")

	var run transferArgs
	var runSigner, runKeySource, runPrivateKey, runPollInterval, runStepTimeout string
	var runGasMultiplier float64
	var runMaxFeeGwei, runMaxPriorityFeeGwei string
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Plan and execute an ERC-20 transfer action",
		RunE: func(cmd *cobra.Command, _ []string) error {
			txSigner, runSenderAddress, err := resolveRunSignerAndFromAddress(runSigner, runKeySource, runPrivateKey, run.fromAddress)
			if err != nil {
				return err
			}
			runArgs := run
			runArgs.fromAddress = runSenderAddress

			start := time.Now()
			action, err := buildAction(runArgs)
			status := []model.ProviderStatus{{Name: "native", Status: statusFromErr(err), LatencyMS: time.Since(start).Milliseconds()}}
			if err != nil {
				s.captureCommandDiagnostics(nil, status, false)
				return err
			}
			if err := s.ensureActionStore(); err != nil {
				return err
			}
			if err := s.actionStore.Save(action); err != nil {
				return clierr.Wrap(clierr.CodeInternal, "persist planned action", err)
			}
			execOpts, err := parseExecuteOptions(
				run.simulate,
				runPollInterval,
				runStepTimeout,
				runGasMultiplier,
				runMaxFeeGwei,
				runMaxPriorityFeeGwei,
				false,
				false,
			)
			if err != nil {
				s.captureCommandDiagnostics(nil, status, false)
				return err
			}
			if err := s.executeActionWithTimeout(&action, txSigner, execOpts); err != nil {
				s.captureCommandDiagnostics(nil, status, false)
				return err
			}
			s.captureCommandDiagnostics(nil, status, false)
			return s.emitSuccess(trimRootPath(cmd.CommandPath()), action, nil, cacheMetaBypass(), status, false)
		},
	}
	runCmd.Flags().StringVar(&run.chainArg, "chain", "", "Chain identifier")
	runCmd.Flags().StringVar(&run.assetArg, "asset", "", "Asset symbol/address/CAIP-19")
	runCmd.Flags().StringVar(&run.amountBase, "amount", "", "Amount in base units")
	runCmd.Flags().StringVar(&run.amountDecimal, "amount-decimal", "", "Amount in decimal units")
	runCmd.Flags().StringVar(&run.fromAddress, "from-address", "", "Sender EOA address (defaults to signer address)")
	runCmd.Flags().StringVar(&run.recipient, "recipient", "", "Recipient EOA address")
	runCmd.Flags().BoolVar(&run.simulate, "simulate", true, "Run preflight simulation before submission")
	runCmd.Flags().StringVar(&run.rpcURL, "rpc-url", "", "RPC URL override for the selected chain")
	runCmd.Flags().StringVar(&runSigner, "signer", "local", "Signer backend (local)")
	runCmd.Flags().StringVar(&runKeySource, "key-source", execsigner.KeySourceAuto, "Key source (auto|env|file|keystore)")
	runCmd.Flags().StringVar(&runPrivateKey, "private-key", "", "Private key hex override for local signer (less safe)")
	runCmd.Flags().StringVar(&runPollInterval, "poll-interval", "2s", "Receipt polling interval")
	runCmd.Flags().StringVar(&runStepTimeout, "step-timeout", "2m", "Per-step receipt timeout")
	runCmd.Flags().Float64Var(&runGasMultiplier, "gas-multiplier", 1.2, "Gas estimate safety multiplier")
	runCmd.Flags().StringVar(&runMaxFeeGwei, "max-fee-gwei", "", "Optional EIP-1559 max fee (gwei)")
	runCmd.Flags().StringVar(&runMaxPriorityFeeGwei, "max-priority-fee-gwei", "", "Optional EIP-1559 max priority fee (gwei)")
	_ = runCmd.MarkFlagRequired("chain")
	_ = runCmd.MarkFlagRequired("asset")
	_ = runCmd.MarkFlagRequired("recipient")

	var submitActionID string
	var submitSimulate bool
	var submitSigner, submitKeySource, submitPrivateKey, submitFromAddress, submitPollInterval, submitStepTimeout string
	var submitGasMultiplier float64
	var submitMaxFeeGwei, submitMaxPriorityFeeGwei string
	submitCmd := &cobra.Command{
		Use:   "submit",
		Short: "Execute an existing ERC-20 transfer action",
		RunE: func(cmd *cobra.Command, _ []string) error {
			actionID, err := resolveActionID(submitActionID)
			if err != nil {
				return err
			}
			if err := s.ensureActionStore(); err != nil {
				return err
			}
			action, err := s.actionStore.Get(actionID)
			if err != nil {
				return clierr.Wrap(clierr.CodeUsage, "load action", err)
			}
			if action.IntentType != "transfer" {
				return clierr.New(clierr.CodeUsage, "action is not a transfer intent")
			}
			if action.Status == execution.ActionStatusCompleted {
				return s.emitSuccess(trimRootPath(cmd.CommandPath()), action, []string{"action already completed"}, cacheMetaBypass(), nil, false)
			}
			txSigner, err := newExecutionSigner(submitSigner, submitKeySource, submitPrivateKey)
			if err != nil {
				return err
			}
			if strings.TrimSpace(submitFromAddress) != "" && !strings.EqualFold(strings.TrimSpace(submitFromAddress), txSigner.Address().Hex()) {
				return clierr.New(clierr.CodeSigner, "signer address does not match --from-address")
			}
			if strings.TrimSpace(action.FromAddress) != "" && !strings.EqualFold(strings.TrimSpace(action.FromAddress), txSigner.Address().Hex()) {
				return clierr.New(clierr.CodeSigner, "signer address does not match planned action sender")
			}
			execOpts, err := parseExecuteOptions(
				submitSimulate,
				submitPollInterval,
				submitStepTimeout,
				submitGasMultiplier,
				submitMaxFeeGwei,
				submitMaxPriorityFeeGwei,
				false,
				false,
			)
			if err != nil {
				return err
			}
			if err := s.executeActionWithTimeout(&action, txSigner, execOpts); err != nil {
				return err
			}
			return s.emitSuccess(trimRootPath(cmd.CommandPath()), action, nil, cacheMetaBypass(), nil, false)
		},
	}
	submitCmd.Flags().StringVar(&submitActionID, "action-id", "", "Action identifier")
	submitCmd.Flags().BoolVar(&submitSimulate, "simulate", true, "Run preflight simulation before submission")
	submitCmd.Flags().StringVar(&submitSigner, "signer", "local", "Signer backend (local)")
	submitCmd.Flags().StringVar(&submitKeySource, "key-source", execsigner.KeySourceAuto, "Key source (auto|env|file|keystore)")
	submitCmd.Flags().StringVar(&submitPrivateKey, "private-key", "", "Private key hex override for local signer (less safe)")
	submitCmd.Flags().StringVar(&submitFromAddress, "from-address", "", "Expected sender EOA address")
	submitCmd.Flags().StringVar(&submitPollInterval, "poll-interval", "2s", "Receipt polling interval")
	submitCmd.Flags().StringVar(&submitStepTimeout, "step-timeout", "2m", "Per-step receipt timeout")
	submitCmd.Flags().Float64Var(&submitGasMultiplier, "gas-multiplier", 1.2, "Gas estimate safety multiplier")
	submitCmd.Flags().StringVar(&submitMaxFeeGwei, "max-fee-gwei", "", "Optional EIP-1559 max fee (gwei)")
	submitCmd.Flags().StringVar(&submitMaxPriorityFeeGwei, "max-priority-fee-gwei", "", "Optional EIP-1559 max priority fee (gwei)")

	var statusActionID string
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Get transfer action status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			actionID, err := resolveActionID(statusActionID)
			if err != nil {
				return err
			}
			if err := s.ensureActionStore(); err != nil {
				return err
			}
			action, err := s.actionStore.Get(actionID)
			if err != nil {
				return clierr.Wrap(clierr.CodeUsage, "load action", err)
			}
			if action.IntentType != "transfer" {
				return clierr.New(clierr.CodeUsage, "action is not a transfer intent")
			}
			return s.emitSuccess(trimRootPath(cmd.CommandPath()), action, nil, cacheMetaBypass(), nil, false)
		},
	}
	statusCmd.Flags().StringVar(&statusActionID, "action-id", "", "Action identifier")

	root.AddCommand(planCmd)
	root.AddCommand(runCmd)
	root.AddCommand(submitCmd)
	root.AddCommand(statusCmd)
	return root
}
