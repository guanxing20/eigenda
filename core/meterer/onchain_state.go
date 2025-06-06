package meterer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"

	"github.com/Layr-Labs/eigenda/core"
	"github.com/Layr-Labs/eigenda/core/eth"
	"github.com/Layr-Labs/eigensdk-go/logging"
	gethcommon "github.com/ethereum/go-ethereum/common"
)

// PaymentAccounts (For reservations and on-demand payments)

// OnchainPaymentState is an interface for getting information about the current chain state for payments.
type OnchainPayment interface {
	RefreshOnchainPaymentState(ctx context.Context) error
	GetReservedPaymentByAccountAndQuorums(ctx context.Context, accountID gethcommon.Address, quorumNumbers []core.QuorumID) (map[core.QuorumID]*core.ReservedPayment, error)
	GetReservedPaymentByAccount(ctx context.Context, accountID gethcommon.Address) (map[core.QuorumID]*core.ReservedPayment, error)
	GetOnDemandPaymentByAccount(ctx context.Context, accountID gethcommon.Address) (*core.OnDemandPayment, error)
	GetOnDemandQuorumNumbers(ctx context.Context) ([]uint8, error)
	GetGlobalSymbolsPerSecond() uint64
	GetGlobalRatePeriodInterval() uint64
	GetMinNumSymbols() uint64
	GetPricePerSymbol() uint64
	GetReservationWindow() uint64
}

var _ OnchainPayment = (*OnchainPaymentState)(nil)

type OnchainPaymentState struct {
	tx     *eth.Reader
	logger logging.Logger

	ReservedPayments map[gethcommon.Address]map[core.QuorumID]*core.ReservedPayment
	OnDemandPayments map[gethcommon.Address]*core.OnDemandPayment

	ReservationsLock sync.RWMutex
	OnDemandLocks    sync.RWMutex

	PaymentVaultParams atomic.Pointer[PaymentVaultParams]
}

type PaymentVaultParams struct {
	GlobalSymbolsPerSecond   uint64
	GlobalRatePeriodInterval uint64
	MinNumSymbols            uint64
	PricePerSymbol           uint64
	ReservationWindow        uint64
	OnDemandQuorumNumbers    []uint8
}

func NewOnchainPaymentState(ctx context.Context, tx *eth.Reader, logger logging.Logger) (*OnchainPaymentState, error) {
	state := OnchainPaymentState{
		tx:                 tx,
		logger:             logger.With("component", "OnchainPaymentState"),
		ReservedPayments:   make(map[gethcommon.Address]map[core.QuorumID]*core.ReservedPayment),
		OnDemandPayments:   make(map[gethcommon.Address]*core.OnDemandPayment),
		PaymentVaultParams: atomic.Pointer[PaymentVaultParams]{},
	}

	paymentVaultParams, err := state.GetPaymentVaultParams(ctx)
	if err != nil {
		return nil, err
	}

	state.PaymentVaultParams.Store(paymentVaultParams)

	return &state, nil
}

func (pcs *OnchainPaymentState) GetPaymentVaultParams(ctx context.Context) (*PaymentVaultParams, error) {
	blockNumber, err := pcs.tx.GetCurrentBlockNumber(ctx)
	if err != nil {
		return nil, err
	}
	quorumNumbers, err := pcs.tx.GetRequiredQuorumNumbers(ctx, blockNumber)
	if err != nil {
		return nil, err
	}

	globalSymbolsPerSecond, err := pcs.tx.GetGlobalSymbolsPerSecond(ctx, blockNumber)
	if err != nil {
		return nil, err
	}

	globalRatePeriodInterval, err := pcs.tx.GetGlobalRatePeriodInterval(ctx, blockNumber)
	if err != nil {
		return nil, err
	}

	minNumSymbols, err := pcs.tx.GetMinNumSymbols(ctx, blockNumber)
	if err != nil {
		return nil, err
	}

	pricePerSymbol, err := pcs.tx.GetPricePerSymbol(ctx, blockNumber)
	if err != nil {
		return nil, err
	}

	reservationWindow, err := pcs.tx.GetReservationWindow(ctx, blockNumber)
	if err != nil {
		return nil, err
	}

	return &PaymentVaultParams{
		OnDemandQuorumNumbers:    quorumNumbers,
		GlobalSymbolsPerSecond:   globalSymbolsPerSecond,
		GlobalRatePeriodInterval: globalRatePeriodInterval,
		MinNumSymbols:            minNumSymbols,
		PricePerSymbol:           pricePerSymbol,
		ReservationWindow:        reservationWindow,
	}, nil
}

// RefreshOnchainPaymentState returns the current onchain payment state
func (pcs *OnchainPaymentState) RefreshOnchainPaymentState(ctx context.Context) error {
	paymentVaultParams, err := pcs.GetPaymentVaultParams(ctx)
	if err != nil {
		return err
	}
	// These parameters should be rarely updated, but we refresh them anyway
	pcs.PaymentVaultParams.Store(paymentVaultParams)

	var refreshErr error
	if reservedPaymentsErr := pcs.refreshReservedPayments(ctx); reservedPaymentsErr != nil {
		pcs.logger.Error("failed to refresh reserved payments", "error", reservedPaymentsErr)
		refreshErr = errors.Join(refreshErr, reservedPaymentsErr)
	}

	if ondemandPaymentsErr := pcs.refreshOnDemandPayments(ctx); ondemandPaymentsErr != nil {
		pcs.logger.Error("failed to refresh on-demand payments", "error", ondemandPaymentsErr)
		refreshErr = errors.Join(refreshErr, ondemandPaymentsErr)
	}

	return refreshErr
}

func (pcs *OnchainPaymentState) refreshReservedPayments(ctx context.Context) error {
	pcs.ReservationsLock.Lock()
	defer pcs.ReservationsLock.Unlock()

	if len(pcs.ReservedPayments) == 0 {
		pcs.logger.Info("No reserved payments to refresh")
		return nil
	}

	accountIDs := make([]gethcommon.Address, 0, len(pcs.ReservedPayments))
	for accountID := range pcs.ReservedPayments {
		accountIDs = append(accountIDs, accountID)
	}

	reservedPayments, err := pcs.tx.GetReservedPayments(ctx, accountIDs)
	if err != nil {
		return err
	}
	pcs.ReservedPayments = reservedPayments
	return nil
}

func (pcs *OnchainPaymentState) refreshOnDemandPayments(ctx context.Context) error {
	pcs.OnDemandLocks.Lock()
	defer pcs.OnDemandLocks.Unlock()

	if len(pcs.OnDemandPayments) == 0 {
		pcs.logger.Info("No on-demand payments to refresh")
		return nil
	}

	accountIDs := make([]gethcommon.Address, 0, len(pcs.OnDemandPayments))
	for accountID := range pcs.OnDemandPayments {
		accountIDs = append(accountIDs, accountID)
	}

	onDemandPayments, err := pcs.tx.GetOnDemandPayments(ctx, accountIDs)
	if err != nil {
		return err
	}
	pcs.OnDemandPayments = onDemandPayments
	return nil
}

// GetReservedPaymentByAccountAndQuorums returns a pointer to the active reservation for the given account ID; no writes will be made to the reservation
func (pcs *OnchainPaymentState) GetReservedPaymentByAccountAndQuorums(ctx context.Context, accountID gethcommon.Address, quorumNumbers []core.QuorumID) (map[core.QuorumID]*core.ReservedPayment, error) {
	pcs.ReservationsLock.RLock()
	if quorumReservations, ok := (pcs.ReservedPayments)[accountID]; ok {
		// Check if all the quorums are present; pull the chain state if not
		allFound := true
		for _, quorumNumber := range quorumNumbers {
			if _, ok := quorumReservations[quorumNumber]; !ok {
				allFound = false
				break
			}
		}
		if allFound {
			pcs.ReservationsLock.RUnlock()
			return quorumReservations, nil
		}
	}
	pcs.ReservationsLock.RUnlock()

	// pulls the chain state
	// TODO: update this to be pulling specific quorum IDs from the chain
	res, err := pcs.tx.GetReservedPaymentByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	pcs.ReservationsLock.Lock()
	// update specific quorum reservations
	if (pcs.ReservedPayments)[accountID] == nil {
		(pcs.ReservedPayments)[accountID] = make(map[core.QuorumID]*core.ReservedPayment)
	}
	for _, quorumNumber := range quorumNumbers {
		if _, ok := res[quorumNumber]; ok {
			(pcs.ReservedPayments)[accountID][quorumNumber] = res[quorumNumber]
		}
	}
	pcs.ReservationsLock.Unlock()

	return res, nil
}

// GetReservedPaymentByAccount returns a pointer to all quorums' reservation for the given account ID; no writes will be made to the reservation
func (pcs *OnchainPaymentState) GetReservedPaymentByAccount(ctx context.Context, accountID gethcommon.Address) (map[core.QuorumID]*core.ReservedPayment, error) {
	pcs.ReservationsLock.RLock()
	if reservation, ok := (pcs.ReservedPayments)[accountID]; ok {
		pcs.ReservationsLock.RUnlock()
		return reservation, nil
	}
	pcs.ReservationsLock.RUnlock()

	// pulls the chain state
	res, err := pcs.tx.GetReservedPaymentByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	pcs.ReservationsLock.Lock()
	(pcs.ReservedPayments)[accountID] = res
	pcs.ReservationsLock.Unlock()

	return res, nil
}

// GetOnDemandPaymentByAccount returns a pointer to the on-demand payment for the given account ID; no writes will be made to the payment
func (pcs *OnchainPaymentState) GetOnDemandPaymentByAccount(ctx context.Context, accountID gethcommon.Address) (*core.OnDemandPayment, error) {
	pcs.OnDemandLocks.RLock()
	if payment, ok := (pcs.OnDemandPayments)[accountID]; ok {
		pcs.OnDemandLocks.RUnlock()
		return payment, nil
	}
	pcs.OnDemandLocks.RUnlock()

	// pulls the chain state
	res, err := pcs.tx.GetOnDemandPaymentByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}

	pcs.OnDemandLocks.Lock()
	(pcs.OnDemandPayments)[accountID] = res
	pcs.OnDemandLocks.Unlock()
	return res, nil
}

func (pcs *OnchainPaymentState) GetOnDemandQuorumNumbers(ctx context.Context) ([]uint8, error) {
	blockNumber, err := pcs.tx.GetCurrentBlockNumber(ctx)
	if err != nil {
		return nil, err
	}

	quorumNumbers, err := pcs.tx.GetRequiredQuorumNumbers(ctx, blockNumber)
	if err != nil {
		// On demand required quorum is unlikely to change, so we are comfortable using the cached value
		// in case the contract read fails
		log.Println("Failed to get required quorum numbers, read from cache", "error", err)
		params := pcs.PaymentVaultParams.Load()
		if params == nil {
			log.Println("Failed to get required quorum numbers and no cached params")
			return nil, fmt.Errorf("failed to get required quorum numbers and no cached params")
		}
		// params.OnDemandQuorumNumbers could be empty if set by the protocol
		return params.OnDemandQuorumNumbers, nil
	}
	return quorumNumbers, nil
}

func (pcs *OnchainPaymentState) GetGlobalSymbolsPerSecond() uint64 {
	params := pcs.PaymentVaultParams.Load()
	if params == nil {
		return 0
	}
	return params.GlobalSymbolsPerSecond
}

func (pcs *OnchainPaymentState) GetGlobalRatePeriodInterval() uint64 {
	params := pcs.PaymentVaultParams.Load()
	if params == nil {
		return 0
	}
	return params.GlobalRatePeriodInterval
}

func (pcs *OnchainPaymentState) GetMinNumSymbols() uint64 {
	params := pcs.PaymentVaultParams.Load()
	if params == nil {
		return math.MaxUint64
	}
	return params.MinNumSymbols
}

func (pcs *OnchainPaymentState) GetPricePerSymbol() uint64 {
	params := pcs.PaymentVaultParams.Load()
	if params == nil {
		return math.MaxUint64
	}
	return params.PricePerSymbol
}

func (pcs *OnchainPaymentState) GetReservationWindow() uint64 {
	params := pcs.PaymentVaultParams.Load()
	if params == nil {
		return 0
	}
	return params.ReservationWindow
}
