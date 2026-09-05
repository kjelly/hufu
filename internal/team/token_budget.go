package team

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
	return owner.reserve(amount)
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
	return owner.commit(reservation, total)
}

func (c *Coordinator) releaseTokenStep(reservation *tokenStepReservation) {
	c.commitTokenStep(reservation, 0)
}
