/*
NotifyWebhooksFunction is triggered by SNS when a player is detected in game.
It dispatches notifications to all configured channels (Discord, Email).
Channels with empty env vars are skipped. Errors per channel are logged but
don't fail the Lambda to prevent SNS retries from sending duplicates.
*/
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/ses/types"
)

// SESAPI is the interface for SES operations used by this function.
type SESAPI interface {
	SendEmail(ctx context.Context, params *ses.SendEmailInput, optFns ...func(*ses.Options)) (*ses.SendEmailOutput, error)
}

// HTTPClient is the interface for making HTTP requests.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// AppConfig holds configuration values loaded from environment variables.
type AppConfig struct {
	DiscordWebhookURL string
	SESSenderEmail    string
	RecipientEmail    string
}

// App holds the dependencies for the Lambda function.
type App struct {
	ses  SESAPI
	http HTTPClient
	cfg  AppConfig
}

type NotificationMessage struct {
	PlayerName    string `json:"playerName"`
	GameMode      string `json:"gameMode"`
	ChampionID    int64  `json:"championId"`
	GameStartTime int64  `json:"gameStartTime"`
}

func loadConfig() AppConfig {
	return AppConfig{
		DiscordWebhookURL: os.Getenv("DISCORD_WEBHOOK_URL"),
		SESSenderEmail:    os.Getenv("SES_SENDER_EMAIL"),
		RecipientEmail:    os.Getenv("RECIPIENT_EMAIL"),
	}
}

func (app *App) handler(ctx context.Context, snsEvent events.SNSEvent) error {
	for _, record := range snsEvent.Records {
		var msg NotificationMessage
		if err := json.Unmarshal([]byte(record.SNS.Message), &msg); err != nil {
			log.Printf("ERROR: failed to parse SNS message: %v", err)
			continue
		}

		if app.cfg.DiscordWebhookURL != "" {
			if err := app.sendDiscord(msg); err != nil {
				log.Printf("ERROR: Discord notification failed: %v", err)
			}
		}

		if app.cfg.SESSenderEmail != "" && app.cfg.RecipientEmail != "" {
			if err := app.sendEmail(ctx, msg); err != nil {
				log.Printf("ERROR: Email notification failed: %v", err)
			}
		}
	}

	return nil
}

// sendDiscord posts an embed message to a Discord webhook.
func (app *App) sendDiscord(msg NotificationMessage) error {
	timestamp := time.UnixMilli(msg.GameStartTime).UTC().Format(time.RFC3339)

	payload := map[string]interface{}{
		"embeds": []map[string]interface{}{
			{
				"title": fmt.Sprintf("%s is in a game!", msg.PlayerName),
				"fields": []map[string]interface{}{
					{"name": "Game Mode", "value": msg.GameMode, "inline": true},
					{"name": "Champion ID", "value": fmt.Sprintf("%d", msg.ChampionID), "inline": true},
				},
				"timestamp": timestamp,
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal Discord payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, app.cfg.DiscordWebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create Discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.http.Do(req)
	if err != nil {
		return fmt.Errorf("failed to POST to Discord: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Discord webhook returned status %d", resp.StatusCode)
	}

	log.Printf("Discord notification sent for %s", msg.PlayerName)
	return nil
}

// sendEmail sends a game detection notification via SES.
func (app *App) sendEmail(ctx context.Context, msg NotificationMessage) error {
	subject := fmt.Sprintf("%s is in a game!", msg.PlayerName)
	body := fmt.Sprintf("%s is currently in a %s game (Champion ID: %d, Start Time: %d)",
		msg.PlayerName, msg.GameMode, msg.ChampionID, msg.GameStartTime)

	_, err := app.ses.SendEmail(ctx, &ses.SendEmailInput{
		Source: &app.cfg.SESSenderEmail,
		Destination: &types.Destination{
			ToAddresses: []string{app.cfg.RecipientEmail},
		},
		Message: &types.Message{
			Subject: &types.Content{Data: &subject},
			Body: &types.Body{
				Text: &types.Content{Data: &body},
			},
		},
	})
	return err
}

func main() {
	cfg := loadConfig()

	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	app := &App{
		ses:  ses.NewFromConfig(awsCfg),
		http: http.DefaultClient,
		cfg:  cfg,
	}

	lambda.Start(app.handler)
}
