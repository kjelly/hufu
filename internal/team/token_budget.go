package team

import "fmt"

// tokenStepReservation is held from PrepareStep until the corresponding
// provider stream reports its usage. Reservations keep concurrently admitted
// model steps from all observing the same remaining budget and starting more
// work than the run can safely absorb.
type tokenStepReservation struct {
	amount  int64
	settled bool
}

type tokenStepAdmission struct {
	reservation   tokenStepReservation
	requestTokens int64
}

func (c *Coordinator) reserveTokenStep(amount int64) (tokenStepReservation, error) {
	owner := c.tokenBudgetRoot()
	if owner == nil || amount <= 0 {
		return tokenStepReservation{}, nil
	}

	owner.tokenBudgetMu.Lock()
	defer owner.tokenBudgetMu.Unlock()
	if owner.tokenBudget <= 0 {
		return tokenStepReservation{}, nil
	}
	used := owner.tokensUsed.Load()
	if used+owner.tokenReservations+amount > owner.tokenBudget {
		return tokenStepReservation{}, fmt.Errorf("token budget admission refused (%d requested, %d used, %d reserved, limit %d)", amount, used, owner.tokenReservations, owner.tokenBudget)
	}
	owner.tokenReservations += amount
	return tokenStepReservation{amount: amount}, nil
}

// commitTokenStep releases the admission reservation and charges exactly the
// provider-reported TotalTokens once. A zero total uses the caller's bounded
// fallback estimate for providers that omit usage.
func (c *Coordinator) commitTokenStep(reservation *tokenStepReservation, total int64) bool {
	if reservation == nil {
		return false
	}
	owner := c.tokenBudgetRoot()
	if owner == nil {
		if reservation.settled {
			return false
		}
		reservation.amount = 0
		reservation.settled = true
		return true
	}
	owner.tokenBudgetMu.Lock()
	defer owner.tokenBudgetMu.Unlock()
	if reservation.settled {
		return false
	}
	if reservation.amount > 0 {
		owner.tokenReservations -= reservation.amount
		if owner.tokenReservations < 0 {
			owner.tokenReservations = 0
		}
	}
	reservation.amount = 0
	reservation.settled = true
	if total > 0 {
		owner.tokensUsed.Add(total)
	}
	return true
}

func (c *Coordinator) releaseTokenStep(reservation *tokenStepReservation) {
	c.commitTokenStep(reservation, 0)
}
