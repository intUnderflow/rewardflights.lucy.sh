package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/alertstore"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/telegram"
	"github.com/intUnderflow/rewardflights.lucy.sh/processor/internal/tglink"
)

const botHelp = `I send award-seat alerts from rewardflights.lucy.sh.

Set an alert on the site (tap the bell on any route) and choose
"Get alerts in Telegram" — that links this chat.

/list — what this chat is watching
/stop — turn off every alert for this chat
/help — this message`

// botHandler answers the bot's four commands. Everything watch-shaped is
// configured on the SITE and arrives via a /start deep-link code; the chat
// is for delivery and off-switches, so there is no command grammar to learn
// (or to validate twice).
func botHandler(ctx context.Context, tg *telegram.Client, store *alertstore.Store, pending *tglink.Pending, logf func(string, ...any)) func(int64, string) {
	reply := func(chatID int64, text string) {
		if err := tg.Reply(ctx, chatID, text); err != nil {
			logf("WARN telegram-reply %s: %v", telegram.ChatLabel(chatID), err)
		}
	}
	endpoint := func(chatID int64) string {
		return fmt.Sprintf("%s%d", alertstore.TelegramPrefix, chatID)
	}
	return func(chatID int64, text string) {
		cmd, arg, _ := strings.Cut(strings.TrimSpace(text), " ")
		// In groups, commands arrive as /cmd@BotName.
		cmd, _, _ = strings.Cut(cmd, "@")
		switch cmd {
		case "/start":
			arg = strings.TrimSpace(arg)
			if arg == "" {
				reply(chatID, botHelp)
				return
			}
			watches, ok := pending.Take(arg)
			if !ok {
				reply(chatID, "That link has expired (they last 10 minutes). Go back to the site and tap the Telegram button again.")
				return
			}
			all, err := store.UpsertTelegram(chatID, watches)
			if err != nil {
				logf("WARN telegram-link %s: %v", telegram.ChatLabel(chatID), err)
				reply(chatID, "Couldn't save that alert: "+err.Error())
				return
			}
			lines := []string{"Armed. The moment award space opens, I'll message you here."}
			lines = append(lines, "", "This chat now watches:")
			for _, w := range all {
				lines = append(lines, "• "+w.Summary())
			}
			lines = append(lines, "", "/list shows these anytime; /stop turns them all off.")
			reply(chatID, strings.Join(lines, "\n"))
			logf("telegram-linked %s (%d watches)", telegram.ChatLabel(chatID), len(all))
		case "/list":
			watches := store.Watches(endpoint(chatID))
			if len(watches) == 0 {
				reply(chatID, "This chat has no alerts yet. Set one on rewardflights.lucy.sh — tap the bell on any route and choose Telegram.")
				return
			}
			lines := []string{"This chat watches:"}
			for _, w := range watches {
				lines = append(lines, "• "+w.Summary())
			}
			reply(chatID, strings.Join(lines, "\n"))
		case "/stop":
			store.Remove(endpoint(chatID))
			reply(chatID, "All alerts for this chat are off. Set new ones anytime from rewardflights.lucy.sh.")
			logf("telegram-stopped %s", telegram.ChatLabel(chatID))
		case "/help":
			reply(chatID, botHelp)
		default:
			// Unknown text: stay quiet in groups (the bot would otherwise
			// answer every message), help in private chats.
			if chatID > 0 {
				reply(chatID, botHelp)
			}
		}
	}
}
