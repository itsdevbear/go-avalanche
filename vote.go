package avalanche

// Vote represents a single vote for a target
type Vote struct {
	err  uint32 // this is called "error" in abc for some reason
	hash Hash
}

// NewVote creates a new Vote for the given hash
func NewVote(err uint32, hash Hash) Vote {
	return Vote{err, hash}
}

// GetHash returns the target hash
func (v Vote) GetHash() Hash {
	return v.hash
}

// GetError returns the vote
func (v Vote) GetError() uint32 {
	return v.err
}

// VoteRecord keeps track of a series of votes for a target
type VoteRecord struct {
	votes      uint8
	consider   uint8
	confidence uint16
}

// NewVoteRecord instantiates a new base record for voting on a target
// `accepted` indicates whether or not the initial state should be acceptance
func NewVoteRecord(accepted bool) *VoteRecord {
	return &VoteRecord{
		votes:      0xaa,
		confidence: boolToUint16(accepted),
	}
}

// isAccepted returns whether or not the voted state is acceptance or not
func (vr VoteRecord) isAccepted() bool {
	return (vr.confidence & 0x01) == 1
}

// getConfidence returns the confidence in the current state's finalization
func (vr VoteRecord) getConfidence() uint16 {
	return vr.confidence >> 1
}

// hasFinalized returns whether or not the record has finalized a state
func (vr VoteRecord) hasFinalized() bool {
	return vr.getConfidence() >= AvalancheFinalizationScore
}

// regsiterVote adds a new vote for an item and update confidence accordingly.
// Returns true if the acceptance or finalization state changed.
func (vr *VoteRecord) regsiterVote(err uint32) bool {
	vr.votes = (vr.votes << 1) | boolToUint8(err == 0)
	vr.consider = (vr.consider << 1) | boolToUint8(int32(err) >= 0)

	yes := countBits8(vr.votes&vr.consider&0xff) > 6

	// The round is inconclusive
	if !yes && countBits8((-vr.votes-1)&vr.consider&0xff) <= 6 {
		return false
	}

	// Vote is conclusive and agrees with our current state
	if vr.isAccepted() == yes {
		vr.confidence += 2
		return vr.getConfidence() == AvalancheFinalizationScore
	}

	// Vote is conclusive but does not agree with our current state
	vr.confidence = boolToUint16(yes)

	return true
}

func (vr *VoteRecord) status() (status Status) {
	finalized := vr.hasFinalized()
	accepted := vr.isAccepted()
	switch {
	case !finalized && accepted:
		status = StatusAccepted
	case !finalized && !accepted:
		status = StatusRejected
	case finalized && accepted:
		status = StatusFinalized
	case finalized && !accepted:
		status = StatusInvalid
	}
	return status
}

func countBits8(i uint8) (count int) {
	for ; i > 0; i &= (i - 1) {
		count++
	}
	return count
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func boolToUint16(b bool) uint16 {
	return uint16(boolToUint8(b))
}
