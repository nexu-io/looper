package loops

import (
	"context"

	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	"github.com/nexu-io/looper/internal/storage"
)

func DeliverHITLAnswerForQuestion(ctx context.Context, repos *storage.Repositories, nowISO, loopID, answer, expectedQuestionID string) error {
	// Same requeue + target exclusion as free-text enqueue / API discard+retry.
	unlock := LockLoopRequeue(loopID)
	defer unlock()

	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	if loop.Status != "awaiting_human" {
		return nil
	}
	unlockTarget := LockLoopTarget(LoopTargetGuardKeyFromRecord(*loop))
	defer unlockTarget()
	ask, ok := ReadHITLAsk(loop.MetadataJSON)
	if !ok {
		return nil
	}
	if expectedQuestionID != "" && (ask.Transport != "feishu" || ask.Status != "awaiting" || ask.QuestionMessageID != expectedQuestionID) {
		return nil
	}
	ask.Answer = answer
	ask.Status = "answered"
	ask.AnsweredAt = nowISO
	meta, werr := WriteHITLAsk(loop.MetadataJSON, ask)
	if werr != nil {
		return werr
	}
	meta, werr = loopcondition.Clear(&meta)
	if werr != nil {
		return werr
	}
	updated := *loop
	updated.MetadataJSON = &meta
	updated.Status = "running"
	updated.NextRunAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	_, err = repos.Queue.RequeueLatestCancelledByLoop(ctx, loopID, nowISO)
	return err
}
