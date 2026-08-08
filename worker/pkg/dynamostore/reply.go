package dynamostore

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/justinswe/std/errors"
)

// replyClaimEntityType marks a claim item, which carries no payload of its own.
const replyClaimEntityType = "REPLY_CLAIM"

// defaultReplyClaimTTL matches the worker's default acknowledgement wait, so a repository
// nobody configured still releases claims in time for the first redelivery.
const defaultReplyClaimTTL = 30 * time.Second

// replyHoldTTL is how long an answered message stays claimed. See HoldReply.
//
// It matches discordv1.MaxMessageAge, the queue's own retention: once no copy of a message
// can still be delivered, the claim has nothing left to suppress. The value is repeated
// here rather than imported so the store does not take a dependency on the wire contract
// to express a duration.
const replyHoldTTL = time.Hour

// replyClaimCondition wins the claim when nobody holds it or when the holder's claim has
// lapsed. There is deliberately no "or the holder is me" arm.
//
// One subscription is shared by the workers at every site, so the duplicate copies of a
// Discord message are load-balanced independently and can both land on this same process.
// An owner arm would let the second copy match its own live claim and answer twice, which
// is the duplicate this whole item exists to prevent. Nothing legitimate needs it: a
// message redelivered to this process after a negative acknowledgement arrives at least
// one AckWait after the claim was taken, by which time expires_at has already admitted it.
const replyClaimCondition = "attribute_not_exists(pk) OR expires_at <= :now"

// replyClaim reserves one Discord message for one worker.
//
// It shares the channel partition with that channel's stored messages, so a claim and
// the message it guards live together and expire under the table's existing TTL.
type replyClaim struct {
	PK         string `dynamodbav:"pk"`
	SK         string `dynamodbav:"sk"`
	EntityType string `dynamodbav:"entity_type"`
	Owner      string `dynamodbav:"owner"`
	ExpiresAt  int64  `dynamodbav:"expires_at"`
}

// ClaimReply reports whether this worker won the right to answer one Discord message.
//
// Delivery is at-least-once and an active-active deployment has a Gateway connection at
// every site, so the same message reaches more than one worker as a matter of course
// rather than only during a handover. The conditional write is what makes exactly one of
// them answer.
//
// The claim is taken before generation, not before the reply is posted, so a worker that
// loses costs one conditional write and never calls a model. That only works because the
// claim expires quickly: see SetReplyClaimTTL. A claim this short would leave a duplicate
// arriving late free to answer again, which is what HoldReply exists to close — the two
// are one guarantee and neither holds alone.
func (r *Repository) ClaimReply(ctx context.Context, channelID, messageID string) (bool, error) {
	now := r.now()
	attributes, err := r.encodeReplyClaim(channelID, messageID, now.Add(r.replyClaimTTL))
	if err != nil {
		return false, err
	}
	condition := replyClaimCondition
	if _, err := r.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           &r.table,
		Item:                attributes,
		ConditionExpression: &condition,
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":now": &dynamodbtypes.AttributeValueMemberN{Value: int64String(now.Unix())},
		},
	}); err != nil {
		var taken *dynamodbtypes.ConditionalCheckFailedException
		if errors.As(err, &taken) {
			return false, nil
		}
		return false, errors.Wrap(err, "claim Discord reply")
	}
	return true, nil
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
func (r *Repository) HoldReply(ctx context.Context, channelID, messageID string) error {
	attributes, err := r.encodeReplyClaim(channelID, messageID, r.now().Add(replyHoldTTL))
	if err != nil {
		return err
	}
	if _, err := r.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &r.table,
		Item:      attributes,
	}); err != nil {
		return errors.Wrap(err, "hold Discord reply claim")
	}
	return nil
}

// encodeReplyClaim validates the identifiers and encodes one claim expiring at expiresAt.
func (r *Repository) encodeReplyClaim(
	channelID, messageID string, expiresAt time.Time,
) (map[string]dynamodbtypes.AttributeValue, error) {
	channelID = strings.TrimSpace(channelID)
	messageID = strings.TrimSpace(messageID)
	if channelID == "" {
		return nil, errors.New("channel ID is required")
	}
	if messageID == "" {
		return nil, errors.New("message ID is required")
	}
	attributes, err := attributevalue.MarshalMap(replyClaim{
		PK:         channelPartitionKey(channelID),
		SK:         replySortKey(messageID),
		EntityType: replyClaimEntityType,
		Owner:      r.replyOwner,
		ExpiresAt:  expiresAt.Unix(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "encode reply claim")
	}
	return attributes, nil
}

// replySortKey returns the sort key holding one message's reply claim.
//
// Unlike messageSortKey this is not zero-padded: claims are only ever read by their exact
// key, never ordered against each other.
func replySortKey(messageID string) string { return "REPLY#" + messageID }

// replyClaimOwner identifies this process in the claims it takes.
func replyClaimOwner() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown"
	}
	return host + "/" + strconv.Itoa(os.Getpid())
}
