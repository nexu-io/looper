package workerhost

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/infra/feishucli"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func PrivateFeishuClient(cfg *config.Config) *feishucli.Client {
	if cfg == nil || !cfg.HITL.Enabled || !strings.EqualFold(cfg.HITL.AnswerTransport, "feishu") || cfg.HITL.Feishu == nil {
		return nil
	}
	return &feishucli.Client{Config: *cfg.HITL.Feishu}
}

func PostPrivateFeishuQuestion(ctx context.Context, cfg *config.Config, repos *storage.Repositories, gateway *notify.Gateway, loopID string) error {
	client := PrivateFeishuClient(cfg)
	if client == nil {
		return nil
	}
	unlock := loops.LockLoopRequeue(loopID)
	defer unlock()
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || loop.Status != "awaiting_human" || ask.Transport != "feishu" || ask.QuestionMessageID != "" {
		return nil
	}
	return gateway.SendHITLAsk(ctx, notify.HITLAskCard{
		AskedAt: ask.AskedAt, Transport: "feishu", ProjectID: loop.ProjectID, LoopID: loop.ID, LoopSeq: loop.Seq,
		Question: ask.Question, Options: ask.Options, MentionOpenIds: ask.MentionOpenIDs,
		Recommendation: ask.Recommendation, RecommendedOption: ask.RecommendedOption,
		Consequences: ask.Consequences, Confidence: ask.Confidence,
	})
}

func PollPrivateFeishu(ctx context.Context, input FeishuPollInput) error {
	client := PrivateFeishuClient(input.Config)
	if client == nil || input.Repos == nil {
		return nil
	}
	records, err := input.Repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	for _, loop := range records {
		ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
		if loop.Status != "awaiting_human" || !ok || ask.Transport != "feishu" || ask.Status != "awaiting" {
			continue
		}
		if input.AllowClaim != nil {
			if err := input.AllowClaim(); err != nil {
				return err
			}
		}
		if ask.QuestionMessageID == "" {
			if err := PostPrivateFeishuQuestion(ctx, input.Config, input.Repos, input.NotificationGateway, loop.ID); err != nil {
				return err
			}
			continue
		}
		chatID := client.Config.ChatID
		if assigned, _, ok := loops.ReadFeishuChat(loop.MetadataJSON); ok {
			chatID = assigned
		}
		root, err := input.Repos.FeishuThreads.RootByLoop(ctx, loop.ID)
		if err != nil {
			return err
		}
		messages, err := client.ThreadMessagesInChat(ctx, root, chatID)
		if err != nil {
			return err
		}
		replyConfig := client.Config
		replyConfig.ChatID = chatID
		reply := feishucli.ReplyAfterQuestion(messages, replyConfig, ask.QuestionMessageID)
		if reply.Text != "" {
			nowISO := eventlog.FormatJavaScriptISOString(input.Now().UTC())
			accepted, err := loops.DeliverHITLAnswerForQuestion(ctx, input.Repos, nowISO, loop.ID, reply.Text, ask.QuestionMessageID)
			if err != nil {
				return err
			}
			if !accepted {
				continue
			}
			if input.OnHITLAnswerDelivered != nil {
				input.OnHITLAnswerDelivered(ctx, loop.ID, reply.Text)
			}
			trackingErr := eventlog.Track(ctx, input.Repos, eventlog.AppendInput{ID: "human.answered:" + loop.ID + ":" + ask.QuestionMessageID, EventType: "human.answered", ProjectID: &loop.ProjectID, LoopID: &loop.ID, Payload: map[string]any{"questionMessageId": ask.QuestionMessageID, "messageIds": reply.MessageIDs, "transport": "feishu"}})
			// A reaction acknowledges receipt without interpreting the answer.
			// Failure must not prevent the already-delivered reply from resuming.
			ackCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			ackErr := trackingErr
			for _, id := range reply.MessageIDs {
				receiptErr := client.MarkReplyProcessing(ackCtx, id)
				ackErr = errors.Join(ackErr, receiptErr)
				status := "sent"
				if receiptErr != nil {
					status = "failed"
				}
				trackErr := eventlog.Track(ctx, input.Repos, eventlog.AppendInput{ID: "feishu.receipt:" + id, EventType: "feishu.receipt", ProjectID: &loop.ProjectID, LoopID: &loop.ID, Payload: map[string]any{"messageId": id, "status": status, "transport": "feishu"}})
				ackErr = errors.Join(ackErr, trackErr)
			}
			cancel()
			if ackErr != nil {
				return fmt.Errorf("reply delivered; Feishu processing reaction failed: %w", ackErr)
			}
		}
	}
	return nil
}

type FeishuPollInput struct {
	Config                *config.Config
	Repos                 *storage.Repositories
	NotificationGateway   *notify.Gateway
	Now                   func() time.Time
	AllowClaim            func() error
	OnHITLAnswerDelivered func(context.Context, string, string)
}
