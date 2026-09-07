package repositories

import (
	"context"
	"errors"
	"fmt"

	"github.com/rmotti/payments-boilerplate/internal/adapters/postgres/models"
	app "github.com/rmotti/payments-boilerplate/internal/application/payments"
	domain "github.com/rmotti/payments-boilerplate/internal/domain/payments"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PaymentRepository persists checkout state and enforces its concurrency
// guarantees through the partial unique indexes defined by the migrations.
type PaymentRepository struct {
	db *gorm.DB
}

// NewPaymentRepository binds the repository to a GORM handle.
func NewPaymentRepository(db *gorm.DB) *PaymentRepository {
	return &PaymentRepository{db: db}
}

// PrepareCheckout atomically creates or recovers a payment attempt.
func (r *PaymentRepository) PrepareCheckout(
	ctx context.Context,
	payment domain.Payment,
	attempt domain.Attempt,
) (domain.Payment, domain.Attempt, error) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	var preparedPayment domain.Payment
	var preparedAttempt domain.Attempt
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		preparedPayment, preparedAttempt, err = prepareCheckout(tx, payment, attempt)
		return err
	})
	if err != nil {
		return domain.Payment{}, domain.Attempt{}, err
	}
	return preparedPayment, preparedAttempt, nil
}

func prepareCheckout(tx *gorm.DB, payment domain.Payment, attempt domain.Attempt) (domain.Payment, domain.Attempt, error) {
	existingAttempt, found, err := findAttemptByKey(tx, attempt.IdempotencyKey)
	if err != nil {
		return domain.Payment{}, domain.Attempt{}, err
	}
	if found {
		existingPayment, err := findPayment(tx, existingAttempt.PaymentID)
		if err != nil {
			return domain.Payment{}, domain.Attempt{}, err
		}
		if existingPayment.OrderID != payment.OrderID {
			return domain.Payment{}, domain.Attempt{}, app.ErrIdempotencyKeyConflict
		}
		return existingPayment, existingAttempt, nil
	}

	preparedPayment, err := createOrGetActivePayment(tx, payment)
	if err != nil {
		return domain.Payment{}, domain.Attempt{}, err
	}
	row := models.PaymentAttemptFromDomain(attempt)
	row.PaymentID = preparedPayment.ID
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if result.Error != nil {
		return domain.Payment{}, domain.Attempt{}, fmt.Errorf("insert payment attempt: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return preparedPayment, row.ToDomain(), nil
	}

	existingAttempt, found, err = findAttemptByKey(tx, attempt.IdempotencyKey)
	if err != nil {
		return domain.Payment{}, domain.Attempt{}, err
	}
	if found {
		existingPayment, err := findPayment(tx, existingAttempt.PaymentID)
		if err != nil {
			return domain.Payment{}, domain.Attempt{}, err
		}
		if existingPayment.OrderID != payment.OrderID {
			return domain.Payment{}, domain.Attempt{}, app.ErrIdempotencyKeyConflict
		}
		return existingPayment, existingAttempt, nil
	}
	return domain.Payment{}, domain.Attempt{}, app.ErrCheckoutInProgress
}

func createOrGetActivePayment(tx *gorm.DB, payment domain.Payment) (domain.Payment, error) {
	row := models.PaymentFromDomain(payment)
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if result.Error != nil {
		return domain.Payment{}, fmt.Errorf("insert payment: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return payment, nil
	}

	var existing models.Payment
	err := tx.Where("order_id = ? AND status NOT IN ?", payment.OrderID, []string{"failed", "cancelled"}).First(&existing).Error
	if err != nil {
		return domain.Payment{}, fmt.Errorf("find active payment: %w", err)
	}
	return existing.ToDomain(), nil
}

func findPayment(tx *gorm.DB, id string) (domain.Payment, error) {
	var row models.Payment
	if err := tx.First(&row, "id = ?", id).Error; err != nil {
		return domain.Payment{}, fmt.Errorf("find payment: %w", err)
	}
	return row.ToDomain(), nil
}

func findAttemptByKey(tx *gorm.DB, key string) (domain.Attempt, bool, error) {
	var row models.PaymentAttempt
	if err := tx.Where("idempotency_key = ?", key).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return domain.Attempt{}, false, nil
		}
		return domain.Attempt{}, false, fmt.Errorf("find payment attempt: %w", err)
	}
	return row.ToDomain(), true, nil
}

// SaveSession attaches the provider session only while the attempt is still
// in the created state. Repeating the same update is accepted.
func (r *PaymentRepository) SaveSession(ctx context.Context, attempt domain.Attempt) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	result := r.db.WithContext(ctx).Model(&models.PaymentAttempt{}).
		Where("id = ? AND status = ?", attempt.ID, string(domain.AttemptStatusCreated)).
		Updates(map[string]any{
			"status":                     string(attempt.Status),
			"provider_session_id":        attempt.ProviderSessionID,
			"provider_payment_intent_id": nullableString(attempt.ProviderPaymentIntentID),
			"checkout_url":               attempt.CheckoutURL,
			"expires_at":                 attempt.ExpiresAt.UTC(),
			"updated_at":                 attempt.UpdatedAt.UTC(),
		})
	if result.Error != nil {
		return fmt.Errorf("update payment attempt session: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return nil
	}

	var existing models.PaymentAttempt
	if err := r.db.WithContext(ctx).First(&existing, "id = ?", attempt.ID).Error; err != nil {
		return fmt.Errorf("find updated payment attempt: %w", err)
	}
	if existing.ProviderSessionID != nil && *existing.ProviderSessionID == attempt.ProviderSessionID {
		return nil
	}
	return errors.New("payment attempt session changed concurrently")
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
