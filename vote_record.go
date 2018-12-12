package avalanche

type VoteRecord struct {
	votes      uint16
	confidence uint16
}

func NewVoteRecord() *VoteRecord {
	return &VoteRecord{votes: 0xaaaa}
}

func (vr VoteRecord) isAccepted() bool {
	return (vr.confidence & 0x01) == 1
}

func (vr VoteRecord) getConfidence() uint16 {
	return vr.confidence >> 1
}

func (vr VoteRecord) hasFinalized() bool {
	return vr.getConfidence() >= AvalancheFinalizationScore
}

// regsiterVote adds a new vote for an item and update confidence accordingly.
// Returns true if the acceptance or finalization state changed.
func (vr *VoteRecord) regsiterVote(vote bool) bool {
	var voteInt uint16
	if vote {
		voteInt = 1
	}

	vr.votes = (vr.votes << 1) | voteInt

	bitCount := countBits(vr.votes & 0xff)
	yes := (bitCount > 6)
	no := (bitCount < 2)

	// Vote is inconclusive
	if !yes && !no {
		return false
	}

	// Vote is conclusive and agrees with our current state
	if vr.isAccepted() == yes {
		vr.confidence += 2
		return vr.hasFinalized()
	}

	// Vote is conclusive but does not agree with our current state
	vr.confidence = boolToUint16(yes)

	return true
}

func countBits(i uint16) (count int) {
	for ; i > 0; i &= (i - 1) {
		count++
	}
	return count
}

func boolToUint16(b bool) (i uint16) {
	if b {
		i = 1
	}
	return i
}