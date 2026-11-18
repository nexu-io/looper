package collab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/feishucli"
	"github.com/nexu-io/looper/internal/infra/notify"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
	"github.com/nexu-io/looper/internal/worker"
	"github.com/nexu-io/looper/internal/workerhost"
)

const (
	defaultChatStateFile = "default-feishu-chat.json"
)

// taskChatManager owns only chat provisioning and durable routing. It does not
// decide which people should be involved; InviteOpenIDs is an optional seed and
// the agent can continue the conversation with anyone added later.
type taskChatManager struct {
	mu           sync.Mutex // Serialize provisioning so two items cannot create the same scope twice.
	client       *feishucli.Client
	config       config.HITLFeishuConfig
	repos        *storage.Repositories
	defaultState string
}

func newTaskChatManager(cfg config.Config, repos *storage.Repositories, stateDir string) *taskChatManager {
	client := workerhost.PrivateFeishuClient(&cfg)
	if client == nil {
		return nil
	}
	return &taskChatManager{client: client, config: *cfg.HITL.Feishu, repos: repos, defaultState: filepath.Join(stateDir, defaultChatStateFile)}
}

// prepare creates or reuses the chat before the worker starts. Provisioning is
// intentionally idempotent for a persisted loop; a failed Feishu call is
// returned to the caller so the host can log it while still letting the task
// run in the configured fallback chat.
func (m *taskChatManager) prepare(ctx context.Context, loopID string) error {
	if m == nil || m.client == nil || m.repos == nil || m.repos.Loops == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	loop, err := m.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	var input worker.EnqueueInput
	// Retry from the original worker input after restarts, including an empty
	// Plane scope which intentionally uses the default group for isolated bugs.
	if loop.MetadataJSON != nil {
		var saved struct {
			Worker worker.EnqueueInput `json:"worker"`
		}
		if json.Unmarshal([]byte(*loop.MetadataJSON), &saved) == nil {
			input = saved.Worker
		}
	}
	chatID, existingKey, _ := loops.ReadFeishuChat(loop.MetadataJSON)
	key := taskChatKey(*loop, input)
	if key == "" {
		key = existingKey
	}
	groupContext := taskGroupContext(input, key)
	createdChat := false
	// If an earlier attempt already posted in the fallback group, keep that
	// conversation there when Feishu becomes available again.
	if chatID == "" {
		chatID, err = m.repos.FeishuThreads.ChatIDByLoop(ctx, loop.ID)
		if err != nil {
			return err
		}
	}
	if chatID == "" && key != "" {
		existing, err := m.repos.Loops.FeishuChatByScope(ctx, key)
		if err != nil {
			return err
		}
		chatID = existing
	}
	if chatID == "" {
		if key != "" && m.config.AutoCreateTaskChats {
			name := m.groupName(*loop, input, key)
			created, err := m.client.CreateGroup(ctx, name, groupContext.Description(), []string{m.config.OwnerOpenID})
			if err != nil {
				return err
			}
			chatID = created
			createdChat = true
		} else if key == "" {
			chatID = m.readDefaultChat()
			if chatID == "" && strings.TrimSpace(m.config.ChatID) != "" {
				chatID = strings.TrimSpace(m.config.ChatID)
			}
			if chatID == "" && m.config.AutoCreateTaskChats {
				created, err := m.client.CreateGroup(ctx, m.defaultGroupName(), groupContext.Description(), []string{m.config.OwnerOpenID})
				if err != nil {
					return err
				}
				chatID = created
				createdChat = true
				_ = m.writeDefaultChat(chatID)
			}
		} else {
			chatID = strings.TrimSpace(m.config.ChatID)
		}
	}
	if chatID == "" {
		return fmt.Errorf("no Feishu chat available for loop %s", loop.ID)
	}
	if err := m.repos.Loops.AssignFeishuChat(ctx, loop.ID, chatID, key); err != nil {
		return err
	}
	inviteErr := m.client.InviteMembers(ctx, chatID, m.config.InviteOpenIDs)
	if createdChat {
		err := m.client.InitializeAnnouncement(ctx, chatID, groupContext.Kind+" · "+groupContext.Name, groupContext.URL, groupContext.Rule)
		return errors.Join(inviteErr, err)
	}
	return inviteErr
}

func taskGroupContext(input worker.EnqueueInput, key string) notify.FeishuGroupContext {
	if key == "" {
		return notify.FeishuGroupContext{Kind: "默认群", Name: "零散任务", Rule: "未归入 Module 或父需求的任务在本群协作。每项任务单独开话题，进入话题 @bot 沟通；Plane 链接见任务卡片。"}
	}
	kind := "任务"
	if input.Module != "" || strings.Contains(key, ":module:") || strings.HasPrefix(key, "module:") {
		kind = "Module"
	} else if strings.Contains(key, ":requirement:") {
		kind = "需求"
	}
	name := firstNonEmpty(input.ChatTitle, input.Title)
	return notify.FeishuGroupContext{Kind: kind, Name: name, URL: firstNonEmpty(input.ChatURL, input.IssueURL), Rule: "这个范围内的子任务共用本群，每项任务各开独立话题；进展、讨论和验收都回到对应话题。"}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func taskChatKey(loop storage.LoopRecord, input worker.EnqueueInput) string {
	if scope := strings.TrimSpace(input.ChatScope); scope != "" {
		return scope
	}
	if module := strings.TrimSpace(input.Module); module != "" {
		return "module:" + loop.ProjectID + ":" + module
	}
	if input.StrictDispatchID != "" {
		return ""
	}
	if input.IssueNumber > 0 {
		repo := strings.TrimSpace(input.Repo)
		if repo == "" && loop.Repo != nil {
			repo = strings.TrimSpace(*loop.Repo)
		}
		if repo != "" {
			return "issue:" + repo + ":" + strconv.FormatInt(input.IssueNumber, 10)
		}
	}
	if loop.TargetType == "issue" && loop.TargetID != nil {
		if raw := strings.TrimSpace(*loop.TargetID); raw != "" {
			if index := strings.LastIndex(raw, ":"); index >= 0 {
				if number, err := strconv.ParseInt(strings.TrimSpace(raw[index+1:]), 10, 64); err == nil && number > 0 {
					repo := strings.TrimSpace(input.Repo)
					if repo == "" && loop.Repo != nil {
						repo = strings.TrimSpace(*loop.Repo)
					}
					if repo != "" {
						return "issue:" + repo + ":" + strconv.FormatInt(number, 10)
					}
				}
			}
		}
	}
	return ""
}

func (m *taskChatManager) groupName(loop storage.LoopRecord, input worker.EnqueueInput, key string) string {
	prefix := strings.TrimSpace(m.config.ChatNamePrefix)
	if prefix == "" {
		prefix = "Looper"
	}
	title := strings.TrimSpace(input.ChatTitle)
	if title == "" {
		title = strings.TrimSpace(input.Title)
	}
	if title == "" {
		title = key
	}
	return trimRunes(prefix+" · "+taskGroupContext(input, key).Kind+" · "+title, 60)
}

func (m *taskChatManager) defaultGroupName() string {
	prefix := strings.TrimSpace(m.config.ChatNamePrefix)
	if prefix == "" {
		prefix = "Looper"
	}
	return trimRunes(prefix+" · 默认协作", 60)
}

func trimRunes(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > max {
		runes = runes[:max]
	}
	return string(runes)
}

func (m *taskChatManager) readDefaultChat() string {
	data, err := os.ReadFile(m.defaultState)
	if err != nil {
		return ""
	}
	var state struct {
		ChatID string `json:"chatId"`
	}
	if json.Unmarshal(data, &state) != nil {
		return ""
	}
	return strings.TrimSpace(state.ChatID)
}

func (m *taskChatManager) writeDefaultChat(chatID string) error {
	if err := os.MkdirAll(filepath.Dir(m.defaultState), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]string{"chatId": strings.TrimSpace(chatID)})
	if err != nil {
		return err
	}
	tmp := m.defaultState + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, m.defaultState)
}
