package store

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/layer-3/nitewatch/custody"
)

type WithdrawalModel struct {
	gorm.Model
	WithdrawalID string `gorm:"uniqueIndex;type:varchar(66)"`
	User         string `gorm:"index;type:varchar(42)"`
	Token        string `gorm:"index;type:varchar(42)"`
	Amount       string `gorm:"type:text"`
	BlockNumber  uint64
	TxHash       string    `gorm:"type:varchar(66)"`
	Timestamp    time.Time `gorm:"index"`
}

type BlockCursorModel struct {
	StreamName  string    `gorm:"primaryKey;type:varchar(64)"`
	BlockNumber uint64    `gorm:"not null"`
	LogIndex    uint      `gorm:"not null"`
	UpdatedAt   time.Time `gorm:"not null;autoUpdateTime"`
}

type WithdrawEventModel struct {
	ID           uint64    `gorm:"primaryKey;autoIncrement"`
	WithdrawalID string    `gorm:"type:varchar(66);not null;uniqueIndex"`
	UserAddress  string    `gorm:"type:varchar(42);not null"`
	TokenAddress string    `gorm:"type:varchar(42);not null"`
	Amount       string    `gorm:"type:text;not null"`
	Decision     string    `gorm:"type:varchar(16);not null"`
	Reason       string    `gorm:"type:text;not null;default:''"`
	BlockNumber  uint64    `gorm:"not null"`
	TxHash       string    `gorm:"type:varchar(66);not null"`
	LogIndex     uint      `gorm:"not null"`
	CreatedAt    time.Time `gorm:"not null;autoCreateTime"`
}

type PendingRejectionModel struct {
	ID           uint64    `gorm:"primaryKey;autoIncrement"`
	WithdrawalID string    `gorm:"type:varchar(66);not null;uniqueIndex"`
	Reason       string    `gorm:"type:text;not null;default:''"`
	Completed    bool      `gorm:"not null;default:false"`
	RetryCount   int       `gorm:"not null;default:0"`
	CreatedAt    time.Time `gorm:"not null;autoCreateTime"`
}

type PendingFinalizationModel struct {
	ID           uint64    `gorm:"primaryKey;autoIncrement"`
	WithdrawalID string    `gorm:"type:varchar(66);not null;uniqueIndex"`
	UserAddress  string    `gorm:"type:varchar(42);not null"`
	TokenAddress string    `gorm:"type:varchar(42);not null"`
	Amount       string    `gorm:"type:text;not null"`
	Completed    bool      `gorm:"not null;default:false"`
	RetryCount   int       `gorm:"not null;default:0"`
	CreatedAt    time.Time `gorm:"not null;autoCreateTime"`
}

type Adapter struct {
	db *gorm.DB
}

func NewAdapter(db *gorm.DB) (*Adapter, error) {
	if err := db.AutoMigrate(&WithdrawalModel{}, &BlockCursorModel{}, &WithdrawEventModel{}, &PendingRejectionModel{}, &PendingFinalizationModel{}); err != nil {
		return nil, err
	}
	return &Adapter{db: db}, nil
}

var _ custody.WithdrawalStore = (*Adapter)(nil)

func (a *Adapter) Save(w *custody.Withdrawal) error {
	model := &WithdrawalModel{
		WithdrawalID: common.Hash(w.WithdrawalID).Hex(),
		User:         w.User.Hex(),
		Token:        w.Token.Hex(),
		Amount:       w.Amount.String(),
		BlockNumber:  w.BlockNumber,
		TxHash:       w.TxHash.Hex(),
		Timestamp:    w.Timestamp,
	}
	return a.db.Create(model).Error
}

func (a *Adapter) GetTotalWithdrawn(token common.Address, since time.Time) (*big.Int, error) {
	var withdrawals []WithdrawalModel
	if err := a.db.Where("token = ? AND timestamp >= ?", token.Hex(), since).Find(&withdrawals).Error; err != nil {
		return nil, err
	}
	return sumAmounts(withdrawals)
}

func (a *Adapter) GetTotalWithdrawnByUser(user, token common.Address, since time.Time) (*big.Int, error) {
	var withdrawals []WithdrawalModel
	if err := a.db.Where("user = ? AND token = ? AND timestamp >= ?", user.Hex(), token.Hex(), since).Find(&withdrawals).Error; err != nil {
		return nil, err
	}
	return sumAmounts(withdrawals)
}

func sumAmounts(withdrawals []WithdrawalModel) (*big.Int, error) {
	total := new(big.Int)
	for _, w := range withdrawals {
		amount, ok := new(big.Int).SetString(w.Amount, 10)
		if !ok {
			return nil, fmt.Errorf("corrupted amount in withdrawal %s: %q", w.WithdrawalID, w.Amount)
		}
		total.Add(total, amount)
	}
	return total, nil
}

func (a *Adapter) GetCursor(streamName string) (blockNumber uint64, logIndex uint32, err error) {
	var cursor BlockCursorModel
	result := a.db.Where("stream_name = ?", streamName).First(&cursor)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return 0, 0, nil
		}
		return 0, 0, result.Error
	}
	return cursor.BlockNumber, uint32(cursor.LogIndex), nil
}

func (a *Adapter) RecordWithdrawEvent(ev *WithdrawEventModel) error {
	return a.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(ev)
		if result.Error != nil {
			return result.Error
		}
		return upsertCursor(tx, "withdraw_started", ev.BlockNumber, ev.LogIndex)
	})
}

func (a *Adapter) HasWithdrawEvent(withdrawalID string) bool {
	var count int64
	a.db.Model(&WithdrawEventModel{}).Where("withdrawal_id = ?", withdrawalID).Count(&count)
	return count > 0
}

func (a *Adapter) SavePendingRejection(p *PendingRejectionModel) error {
	return a.db.Clauses(clause.OnConflict{DoNothing: true}).Create(p).Error
}

func (a *Adapter) GetPendingRejections() ([]PendingRejectionModel, error) {
	var pending []PendingRejectionModel
	if err := a.db.Where("completed = ?", false).Find(&pending).Error; err != nil {
		return nil, err
	}
	return pending, nil
}

func (a *Adapter) CompletePendingRejection(withdrawalID string) error {
	return a.db.Model(&PendingRejectionModel{}).
		Where("withdrawal_id = ?", withdrawalID).
		Update("completed", true).Error
}

func (a *Adapter) IncrementRejectionRetry(withdrawalID string) error {
	return a.db.Model(&PendingRejectionModel{}).
		Where("withdrawal_id = ?", withdrawalID).
		UpdateColumn("retry_count", gorm.Expr("retry_count + 1")).Error
}

func (a *Adapter) SavePendingFinalization(p *PendingFinalizationModel) error {
	return a.db.Clauses(clause.OnConflict{DoNothing: true}).Create(p).Error
}

func (a *Adapter) GetPendingFinalizations() ([]PendingFinalizationModel, error) {
	var pending []PendingFinalizationModel
	if err := a.db.Where("completed = ?", false).Find(&pending).Error; err != nil {
		return nil, err
	}
	return pending, nil
}

func (a *Adapter) CompletePendingFinalization(withdrawalID string) error {
	return a.db.Model(&PendingFinalizationModel{}).
		Where("withdrawal_id = ?", withdrawalID).
		Update("completed", true).Error
}

func (a *Adapter) IncrementFinalizationRetry(withdrawalID string) error {
	return a.db.Model(&PendingFinalizationModel{}).
		Where("withdrawal_id = ?", withdrawalID).
		UpdateColumn("retry_count", gorm.Expr("retry_count + 1")).Error
}

// RecordWithdrawEventOnly records an event without advancing the cursor.
// Used for recoverable errors so the event can be re-processed on restart.
func (a *Adapter) RecordWithdrawEventOnly(ev *WithdrawEventModel) error {
	return a.db.Clauses(clause.OnConflict{DoNothing: true}).Create(ev).Error
}

// UpdateWithdrawEventDecision updates the decision and reason on an existing event record.
func (a *Adapter) UpdateWithdrawEventDecision(withdrawalID, decision, reason string) error {
	return a.db.Model(&WithdrawEventModel{}).
		Where("withdrawal_id = ?", withdrawalID).
		Updates(map[string]interface{}{"decision": decision, "reason": reason}).Error
}

// UpsertFinalizedCursor updates the cursor for the withdraw_finalized stream.
func (a *Adapter) UpsertFinalizedCursor(blockNumber uint64, logIndex uint) error {
	return upsertCursor(a.db, "withdraw_finalized", blockNumber, logIndex)
}

func upsertCursor(tx *gorm.DB, streamName string, blockNumber uint64, logIndex uint) error {
	cursor := BlockCursorModel{
		StreamName:  streamName,
		BlockNumber: blockNumber,
		LogIndex:    logIndex,
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "stream_name"}},
		DoUpdates: clause.AssignmentColumns([]string{"block_number", "log_index", "updated_at"}),
	}).Create(&cursor).Error
}
