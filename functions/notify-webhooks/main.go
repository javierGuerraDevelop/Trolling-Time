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

type NotificationMessage struct {
	PlayerName    string `json:"playerName"`
	GameMode      string `json:"gameMode"`
	ChampionID    int64  `json:"championId"`
	GameStartTime int64  `json:"gameStartTime"`
}

func handler(ctx context.Context, snsEvent events.SNSEvent) error {
	for _, record := range snsEvent.Records {
		var msg NotificationMessage
		if err := json.Unmarshal([]byte(record.SNS.Message), &msg); err != nil {
			log.Printf("ERROR: failed to parse SNS message: %v", err)
			continue
		}

		if url := os.Getenv("DISCORD_WEBHOOK_URL"); url != "" {
			if err := sendDiscord(url, msg); err != nil {
				log.Printf("ERROR: Discord notification failed: %v", err)
			}
		}

		senderEmail := os.Getenv("SES_SENDER_EMAIL")
		recipientEmail := os.Getenv("RECIPIENT_EMAIL")
		if senderEmail != "" && recipientEmail != "" {
			if err := sendEmail(ctx, senderEmail, recipientEmail, msg); err != nil {
				log.Printf("ERROR: Email notification failed: %v", err)
			}
		}
	}

	return nil
}

// sendDiscord posts an embed message to a Discord webhook.
func sendDiscord(webhookURL string, msg NotificationMessage) error {
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

	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(body))
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
func sendEmail(ctx context.Context, sender, recipient string, msg NotificationMessage) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	subject := fmt.Sprintf("%s is in a game!", msg.PlayerName)
	body := fmt.Sprintf("%s is currently in a %s game (Champion ID: %d, Start Time: %d)",
		msg.PlayerName, msg.GameMode, msg.ChampionID, msg.GameStartTime)

	client := ses.NewFromConfig(cfg)
	_, err = client.SendEmail(ctx, &ses.SendEmailInput{
		Source: &sender,
		Destination: &types.Destination{
			ToAddresses: []string{recipient},
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
	lambda.Start(handler)
}
