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
	SecretsFilename  = "telegram_monitor_bot_secrets.json"
	SettingsFilename = "settings.json"
)

// Secrets holds only the authentication secrets for the bot
type Secrets struct {
	TelegramBotToken string `json:"telegram_bot_token"`
}

// Settings holds the configuration settings for the bot
type Settings struct {
	TelegramChannelID int64 `json:"telegram_channel_id"`
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

func sendToTelegram(botToken string, channelID int64, message string) error {
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return fmt.Errorf("error initializing bot: %v", err)
	}

	msg := tgbotapi.NewMessage(channelID, message)

	_, err = bot.Send(msg)
	if err != nil {
		return fmt.Errorf("error sending message: %v", err)
	}

	return nil
}

func printInstructions() {
	fmt.Printf("Missing required configuration for the %s.\n", ServiceName)
	fmt.Println("\nPlease create the following configuration files:")

	fmt.Println("\n1. Secrets file (for the bot token):")
	fmt.Printf("   Path: %s\n", getDefaultSecretsPath())
	fmt.Println("   Format:")
	fmt.Println(`   {
     "telegram_bot_token": "YOUR_TELEGRAM_BOT_TOKEN"
     }`)
	fmt.Println("   To obtain, create a Telegram bot by talking to @BotFather and get the token")

	fmt.Println("\n2. Settings file (for the channel ID):")
	fmt.Printf("   Path: %s\n", getSettingsPath())
	fmt.Println("   Format:")
	fmt.Println(`   {
     "telegram_channel_id": YOUR_CHANNEL_ID_NUMBER
   }`)
	fmt.Println("   To get it, add your bot to the target channel as an administrator,")
	fmt.Println("   and forward a message from the channel to @userinfobot.")
	fmt.Println("   Use the 'Id' number from the 'Forwarded from chat' value (including the negative sign)")
}

func handleFailure(bot *tgbotapi.BotAPI, channelID int64, failTime time.Time) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	// Try immediately first
	for {
		failText := fmt.Sprintf("Подключение оборвалось %s", failTime.Format(time.RFC1123))
		msgConf := tgbotapi.NewMessage(channelID, failText)

		if _, err := bot.Send(msgConf); err == nil {
			log.Println("Failure message sent successfully")
			return
		} else {
			log.Printf("Error sending fail message: %v", err)
		}

		<-ticker.C
	}
}

// main executes the service
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

	bot, err := tgbotapi.NewBotAPI(secrets.TelegramBotToken)
	if err != nil {
		log.Fatalf("Error initializing bot: %v", err)
	}

	for {
		msgText := fmt.Sprintf("Соединение установлено %s", time.Now().Format(time.RFC1123))
		msgConf := tgbotapi.NewMessage(settings.TelegramChannelID, msgText)

		msg, err := bot.Send(msgConf)
		if err != nil {
			if strings.Contains(err.Error(), "chat not found") {
				log.Fatalf("Failed to send initial message (chat not found): %v", err)
			}
			log.Printf("Failed to send initial message: %v", err)
			handleFailure(bot, settings.TelegramChannelID, time.Now())
			continue
		}

		msgID := msg.MessageID
		ticker := time.NewTicker(time.Minute)
		updateFailed := false

		// Loop to update message
		for range ticker.C {
			updateText := fmt.Sprintf("Связь есть %s (сообщение будет обновляться каждую минуту, пока есть связь)", time.Now().Format("15:04 MST"))
			editMsg := tgbotapi.NewEditMessageText(settings.TelegramChannelID, msgID, updateText)

			if _, err := bot.Send(editMsg); err != nil {
				log.Printf("Error updating message: %v", err)
				updateFailed = true
				ticker.Stop()
				break
			}
		}

		if updateFailed {
			handleFailure(bot, settings.TelegramChannelID, time.Now())
		}
	}
}
