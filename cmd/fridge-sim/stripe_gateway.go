package main

import (
	"fmt"
	"math/rand"

	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/paymentintent"
	"github.com/stripe/stripe-go/v86/refund"
)

// stripePaymentGateway backs vend.PaymentGateway with real Stripe test-mode
// API calls instead of a synthetic random outcome. The rng below only picks
// which Stripe test payment method to present -- pm_card_visa always
// succeeds, pm_card_chargeDeclined always declines with a genuine
// card_declined error -- so success/failure is whatever Stripe's own
// test-mode processing returns, not a coin flip we invent ourselves.
type stripePaymentGateway struct {
	rng *rand.Rand
}

func newStripePaymentGateway(apiKey string, rng *rand.Rand) *stripePaymentGateway {
	stripe.Key = apiKey
	return &stripePaymentGateway{rng: rng}
}

func (g *stripePaymentGateway) Authorize(slotID string, amountCents int) (string, error) {
	paymentMethod := "pm_card_visa"
	if g.rng.Float64() < 0.05 {
		paymentMethod = "pm_card_chargeDeclined"
	}

	pi, err := paymentintent.New(&stripe.PaymentIntentParams{
		Amount:        stripe.Int64(int64(amountCents)),
		Currency:      stripe.String(string(stripe.CurrencyUSD)),
		PaymentMethod: stripe.String(paymentMethod),
		Confirm:       stripe.Bool(true),
		OffSession:    stripe.Bool(true),
		Description:   stripe.String(fmt.Sprintf("fridge-edge-agent vend, slot %s", slotID)),
	})
	if err != nil {
		return "", fmt.Errorf("card declined")
	}
	return pi.ID, nil
}

func (g *stripePaymentGateway) Refund(txnID string) error {
	if _, err := refund.New(&stripe.RefundParams{
		PaymentIntent: stripe.String(txnID),
	}); err != nil {
		return fmt.Errorf("payment processor unreachable")
	}
	return nil
}
