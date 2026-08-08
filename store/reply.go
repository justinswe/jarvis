package store

import (
	"context"
	"time"

	"github.com/justinswe/std/errors"
)

// defaultReplyClaimTTL matches the worker's default acknowledgement wait, so a store
// nobody configured still releases claims in time for the first redelivery.
const defaultReplyClaimTTL = 30 * time.Second

// replyHoldTTL is how long an answered message stays claimed. See HoldReply.
//
// It matches discordv1.MaxMessageAge, the queue's own retention: once no copy of a message
// can still be delivered, the claim has nothing left to suppress. The value is repeated
// here rather than imported so the store does not take a dependency on the wire contract
// to express a duration.
const replyHoldTTL = time.Hour

// SetReplyClaimTTL bounds how long a reply claim blocks other workers.
//
// It must not exceed the broker's redelivery delay. A claim is taken before generation,
// so a worker that dies mid-generation leaves one behind; the redelivery that follows can
// only answer once that claim has lapsed. Set longer than the redelivery delay, every
// attempt is refused until the message is exhausted and the reply is lost. Callers pass
// their acknowledgement wait.
func (s *Store) SetReplyClaimTTL(ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	s.replyClaimTTL = ttl
}

// ClaimReply reports whether this worker won the right to answer one Discord message.
//
// Delivery is at-least-once and an active-active deployment has a Gateway connection at
// every site, so the same message reaches more than one worker as a matter of course
// rather than only during a handover. The conditional upsert is what makes exactly one of
// them answer.
//
// The condition turns on the message alone — absent, or held by a lapsed claim — and
// deliberately has no "or the holder is me" arm. One queue subscription is shared by the
// workers at every site, so both copies of a message can land on this same process; an
// owner arm would let the second copy match its own live claim and answer twice. Nothing
// legitimate needs it: a redelivery to this process arrives at least one AckWait after
// the claim was taken, by which time expires_at has already admitted it.
//
// Expiries race the database's own clock, never a worker's, so two sites disagreeing
// about the time cannot stretch or shrink the claim window.
//
// The claim is taken before generation, not before the reply is posted, so a worker that
// loses costs one write and never calls a model. That only works because the claim
// expires quickly: see SetReplyClaimTTL. A claim this short would leave a duplicate
// arriving late free to answer again, which is what HoldReply exists to close — the two
// are one guarantee and neither holds alone.
func (s *Store) ClaimReply(ctx context.Context, channelID, messageID string) (bool, error) {
	cid, mid, err := claimKey(channelID, messageID)
	if err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO reply_claims (channel_id, message_id, owner, expires_at)
		VALUES (?, ?, ?, @now + ?)
		ON CONFLICT (channel_id, message_id) DO UPDATE
		SET owner = excluded.owner, expires_at = excluded.expires_at
		WHERE reply_claims.expires_at <= @now`),
		cid, mid, s.replyOwner, int64(s.replyClaimTTL/time.Second))
	if err != nil {
		return false, errors.Wrap(err, "claim Discord reply")
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return false, errors.Wrap(err, "confirm Discord reply claim")
	}
	return claimed > 0, nil
}

// HoldReply extends a claim past the point where any duplicate could still be delivered.
//
// ClaimReply deliberately writes a short claim, because a worker that dies mid-generation
// has to let its own redelivery through. That leaves a window a slow duplicate walks into:
// the other site's copy waiting behind a full in-flight budget, or one redelivered after a
// transient failure, reaches a worker after the winner's claim has lapsed and answers a
// second time. Extending once the reply is actually posted closes that window without
// making a crash any more expensive, because a crash never reaches this call.
//
// The write is unconditional. The caller has already answered; refusing to record that
// because some other holder appeared would only invite the duplicate this prevents.
func (s *Store) HoldReply(ctx context.Context, channelID, messageID string) error {
	cid, mid, err := claimKey(channelID, messageID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.q(`
		INSERT INTO reply_claims (channel_id, message_id, owner, expires_at)
		VALUES (?, ?, ?, @now + ?)
		ON CONFLICT (channel_id, message_id) DO UPDATE
		SET owner = excluded.owner, expires_at = excluded.expires_at`),
		cid, mid, s.replyOwner, int64(replyHoldTTL/time.Second))
	return errors.Wrap(err, "hold Discord reply claim")
}

func claimKey(channelID, messageID string) (int64, int64, error) {
	cid, err := snowflake(channelID)
	if err != nil {
		return 0, 0, errors.Wrap(err, "channel ID")
	}
	mid, err := snowflake(messageID)
	if err != nil {
		return 0, 0, errors.Wrap(err, "message ID")
	}
	return cid, mid, nil
}
