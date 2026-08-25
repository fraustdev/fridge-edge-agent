package main

// Failure-rate constants shared by both payment gateway implementations
// (the in-memory simulatedPaymentGateway and the real Stripe-backed one),
// calibrated to plausible real-world ranges rather than picked arbitrarily.
const (
	// paymentDeclineRate is a realistic range for card-present/
	// card-not-present authorization declines generally -- not a
	// Farmer's Fridge-specific figure, which isn't public.
	paymentDeclineRate = 0.06

	// refundFailureRate is the chance a refund attempt itself fails (the
	// payment processor being unreachable on that call) -- this is what
	// produces refund_pending, the "needs manual reconciliation" case, so
	// it's kept deliberately rare relative to paymentDeclineRate: a
	// genuine edge case, not routine noise.
	refundFailureRate = 0.008

	// hardwareFaultInjectRate is the per-vend-attempt chance of injecting
	// a dispenser fault (jam/timeout/sensor error). Tuned low because any
	// single fault permanently marks a fridge "faulted" for this demo's
	// snapshot (see fleet.computeStatus, which never auto-heals a faulted
	// status) -- an earlier, higher rate produced ~88% of a 2,329-fridge
	// fleet reading as faulted at full scale, which reads as a total
	// outage rather than a realistic ops backlog.
	hardwareFaultInjectRate = 0.005
)
