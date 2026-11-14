package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	ServiceName      = "telegram-monitor-bot"
	SecretsFilename  = "host_monitor_bot_secrets.json"
	SettingsFilename = "settings.json"
	// RetryDelay preserves the 1-minute cadence the removed handleFailure loop
	// used between send attempts. The outer loop needs that pacing: after a
	// failed send it must not retry in a tight loop, otherwise the fast-failing
	// requests (DNS/connect refused) hammer the Telegram API and trigger 429s.
	RetryDelay = time.Minute
)

// Secrets holds only the authentication secrets for the bot.
// WhatsApp fields are optional: an empty access token or phone number ID
// disables WhatsApp answering (see newWhatsAppPublisher). The app secret and
// the verify token enable the webhook server, and are ignored without a
// configured publisher.
type Secrets struct {
	TelegramBotToken      string `json:"telegram_bot_token"`
	WhatsAppAccessToken   string `json:"whatsapp_access_token"`
	WhatsAppPhoneNumberID string `json:"whatsapp_phone_number_id"`
	WhatsAppAppSecret     string `json:"whatsapp_app_secret"`
	WebhookVerifyToken    string `json:"webhook_verify_token"`
	YandexDiskClientToken string `json:"yandex_disk_client_token"`
}

// Settings holds the configuration settings for the bot.
// WebhookListen is the loopback address the webhook server binds to, such as
// 127.0.0.1:8080; TLS is terminated in front of it by the Cloudflare Tunnel.
// StatusKeywords is the regular expression matched against text messages from
// participants. An empty expression matches every message.
type Settings struct {
	TelegramChannelID int64  `json:"telegram_channel_id"`
	WebhookListen     string `json:"webhook_listen"`
	StatusKeywords    string `json:"status_keywords"`
}

func getLocalAppDataDir() string {
	// Default paths based on OS
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			log.Panicf("LOCALAPPDATA environment variable is not set")
		}
		return localAppData
	} else {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			log.Panicf("Error getting home directory: %v", err)
		}
		return filepath.Join(homeDir, ".local")
	}
}

func getDefaultSecretsPath() string {
	var secretDataDir string

	if envPath := os.Getenv("SECRETS_PATH"); envPath != "" {
		return envPath
	}

	if dir := os.Getenv("SecretDataDir"); dir != "" {
		secretDataDir = dir
	} else {
		secretDataDir = filepath.Join(getLocalAppDataDir(), "_sec")
	}

	return filepath.Join(secretDataDir, SecretsFilename)
}

func getSettingsPath() string {
	var dataDir string

	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		dataDir = filepath.Join(localAppData, ServiceName)
	} else {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("Error getting home directory: %v", err)
		}
		dataDir = filepath.Join(homeDir, ".local", ServiceName)
	}

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatalf("Error creating data directory: %v", err)
	}

	return filepath.Join(dataDir, SettingsFilename)
}

func loadFile(filePath string, displayType string) ([]byte, error) {
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return nil, fmt.Errorf(`%s file not found at "%s"`, displayType, filePath)
	}

	rawdata, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf(`error %v reading %s file "%s"`, err, displayType, filePath)
	}

	return rawdata, nil
}

func loadSecrets() (*Secrets, error) {
	var secrets Secrets

	rawdata, err := loadFile(getDefaultSecretsPath(), "secrets")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rawdata, &secrets); err != nil {
		return nil, fmt.Errorf("error parsing secrets file: %v", err)
	}

	if secrets.TelegramBotToken == "" {
		return nil, fmt.Errorf("missing required secrets")
	}

	return &secrets, nil
}

func loadSettings() (*Settings, error) {
	var settings Settings

	rawdata, err := loadFile(getSettingsPath(), "settings")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(rawdata, &settings); err != nil {
		return nil, fmt.Errorf("error parsing settings file: %v", err)
	}

	if settings.TelegramChannelID == 0 {
		return nil, fmt.Errorf("missing required settings")
	}

	return &settings, nil
}

func printInstructions() {
	fmt.Printf("Missing required configuration for the %s.\n", ServiceName)
	fmt.Println("\nPlease create the following configuration files:")

	fmt.Println("\n1. Secrets file (for the bot tokens):")
	fmt.Printf("   Path: %s\n", getDefaultSecretsPath())
	fmt.Println("   Format:")
	fmt.Println(`   {
     "telegram_bot_token": "YOUR_TELEGRAM_BOT_TOKEN",
     "whatsapp_access_token": "YOUR_WHATSAPP_ACCESS_TOKEN",
     "whatsapp_phone_number_id": "YOUR_WHATSAPP_PHONE_NUMBER_ID",
     "whatsapp_app_secret": "YOUR_WHATSAPP_APP_SECRET",
     "webhook_verify_token": "YOUR_WEBHOOK_VERIFY_TOKEN",
     "yandex_disk_client_token": "YOUR_YANDEX_DISK_CLIENT_TOKEN"
     }`)
	fmt.Println("   To obtain the Telegram token, create a Telegram bot by talking to @BotFather")
	fmt.Println("   WhatsApp access token and phone number ID are optional. Get them from a Cloud API system")
	fmt.Println("   user in the Meta developer app (WhatsApp > API Setup). Answering needs no Official")
	fmt.Println("   Business Account: the bot replies 1:1, in the 24-hour customer service window that each")
	fmt.Println("   participant's own message opens. Since 2026-10-01 Meta does not deliver service messages")
	fmt.Println("   for a Messaging account with no payment method on file, so add one in WhatsApp Manager.")
	fmt.Println("   The Yandex Disk token is optional. To enable Yandex Disk status uploads, get it at:")
	fmt.Println("   https://oauth.yandex.ru/authorize?response_type=token&client_id=ca79e936dc1646998180d04b87e678b6")

	fmt.Println("\n2. Settings file (for the channel ID and the webhook):")
	fmt.Printf("   Path: %s\n", getSettingsPath())
	fmt.Println("   Format:")
	fmt.Println(`   {
     "telegram_channel_id": YOUR_CHANNEL_ID_NUMBER,
      "webhook_listen": "127.0.0.1:8080",
      "status_keywords": "(?i)^(статус|status)$"
   }`)
	fmt.Println("   To get it, add your bot to the target channel as an administrator,")
	fmt.Println("   and forward a message from the channel to @userinfobot.")
	fmt.Println("   Use the 'Id' number from the 'Forwarded from chat' value (including the negative sign)")
	fmt.Println("   webhook_listen, whatsapp_app_secret and webhook_verify_token are optional, and are used")
	fmt.Println("   only when WhatsApp answering is configured. The webhook server binds to webhook_listen")
	fmt.Println("   on plain HTTP, so TLS is terminated in front of it: for example a Cloudflare Tunnel")
	fmt.Println("   hostname pointing at 127.0.0.1:8080. The Callback URL in the Meta app dashboard must be")
	fmt.Println("   the public HTTPS URL of that hostname plus /webhook, and webhook_verify_token must match")
	fmt.Println("   the Verify token field. The app needs the whatsapp_business_messaging permission, must")
	fmt.Println("   be in Live mode, and must subscribe to the messages field.")
	fmt.Println("   status_keywords is optional: it is a regular expression matched against text messages")
	fmt.Println("   from participants. An omitted or empty expression matches every message.")
}

// monitoringStartMessage is the message that opens the monitoring period. The
// per-minute updates that follow carry the current time, so this text is the
// statement that monitoring began at this moment.
func monitoringStartMessage() string {
	return fmt.Sprintf("Наблюдение начато %s", time.Now().Format(time.RFC1123))
}

func monitoringRetryMessage() string {
	return fmt.Sprintf("Был сбой, переподключение %s", time.Now().Format(time.RFC1123))
}

// main executes the service.
func main() {
	secrets, err := loadSecrets()
	if err != nil {
		log.Printf("Error loading secrets: %v", err)
		printInstructions()
		return
	}

	settings, err := loadSettings()
	if err != nil {
		log.Printf("Error loading settings: %v", err)
		printInstructions()
		return
	}

	publisher := newWhatsAppPublisher(secrets)
	if publisher != nil {
		go runWhatsappResponder(publisher, newWebhookServer(secrets, settings, publisher))
	}
	bot, err := tgbotapi.NewBotAPI(secrets.TelegramBotToken)
	if err != nil {
		log.Fatalf("Error initializing bot: %v", err)
	}

	if secrets.YandexDiskClientToken != "" {
		go runYandexMonitor(secrets.YandexDiskClientToken)
	}

	// msgText is the monitoring start text. The outer loop retries the same
	// message when the Telegram send fails, so the text is not regenerated per
	// iteration: a retried send repeats the same start time, and the per-minute
	// updates that follow carry the current time.
	msgText := monitoringStartMessage()

	for {
		msgConf := tgbotapi.NewMessage(settings.TelegramChannelID, msgText)

		msg, err := bot.Send(msgConf)
		if err != nil {
			if strings.Contains(err.Error(), "chat not found") {
				log.Fatalf("Failed to send initial message (chat not found): %v", err)
			}
			log.Printf("Failed to send initial message: %v", err)
			time.Sleep(RetryDelay)
			continue
		}

		msgID := msg.MessageID
		initialDay := time.Now().YearDay()
		ticker := time.NewTicker(time.Minute)
		updateFailed := false
		timeStr := ""

		// Loop to update message
		for range ticker.C {
			now := time.Now()
			if now.YearDay() == initialDay {
				timeStr = fmt.Sprintf(" %s", now.Format("15:04 MST"))
			} else {
				timeStr = fmt.Sprintf(" %s", now.Format("02.01.2006 15:04 MST"))
			}

			updateText := fmt.Sprintf("Связь есть %s (сообщение будет обновляться каждую минуту, пока есть связь)", timeStr)
			editMsg := tgbotapi.NewEditMessageText(settings.TelegramChannelID, msgID, updateText)

			if _, err := bot.Send(editMsg); err == nil {
				continue
			} else {
				log.Printf("Error updating message: %v", err)
			}

			updateFailed = true
			ticker.Stop()
			break
		}

		if updateFailed {
			// The updates stopped, so the next successful send opens a new
			// monitoring period: the start time is taken here, and the outer loop
			// sends it as the new initial message.
			time.Sleep(RetryDelay)
			msgText = monitoringRetryMessage()
		}
	}
}
