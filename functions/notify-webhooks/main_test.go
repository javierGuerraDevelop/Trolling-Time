package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/ses"
)

// --- Mock implementations ---

type mockSES struct {
	sendEmailFunc func(ctx context.Context, params *ses.SendEmailInput, optFns ...func(*ses.Options)) (*ses.SendEmailOutput, error)
}

func (mock *mockSES) SendEmail(ctx context.Context, params *ses.SendEmailInput, optFns ...func(*ses.Options)) (*ses.SendEmailOutput, error) {
	return mock.sendEmailFunc(ctx, params, optFns...)
}

// --- Helpers ---

func snsEvent(msg NotificationMessage) events.SNSEvent {
	payload, _ := json.Marshal(msg)
	return events.SNSEvent{
		Records: []events.SNSEventRecord{
			{
				SNS: events.SNSEntity{
					Message: string(payload),
				},
			},
		},
	}
}

func testMessage() NotificationMessage {
	return NotificationMessage{
		PlayerName:    "TestPlayer",
		GameMode:      "CLASSIC",
		ChampionID:    99,
		GameStartTime: 1700000000000,
	}
}

// --- Tests ---

func TestHandler_BothChannelsConfigured(t *testing.T) {
	var discordCalled, emailCalled bool

	discordServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		discordCalled = true
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), "TestPlayer is in a game!") {
			t.Errorf("Discord payload missing player name, got: %s", body)
		}
		if req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", req.Header.Get("Content-Type"))
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer discordServer.Close()

	sesClient := &mockSES{
		sendEmailFunc: func(ctx context.Context, params *ses.SendEmailInput, optFns ...func(*ses.Options)) (*ses.SendEmailOutput, error) {
			emailCalled = true
			if *params.Source != "sender@test.com" {
				t.Errorf("expected sender 'sender@test.com', got '%s'", *params.Source)
			}
			if params.Destination.ToAddresses[0] != "recipient@test.com" {
				t.Errorf("expected recipient 'recipient@test.com', got '%s'", params.Destination.ToAddresses[0])
			}
			if !strings.Contains(*params.Message.Subject.Data, "TestPlayer is in a game!") {
				t.Errorf("unexpected email subject: %s", *params.Message.Subject.Data)
			}
			return &ses.SendEmailOutput{}, nil
		},
	}

	app := &App{
		ses:  sesClient,
		http: discordServer.Client(),
		cfg: AppConfig{
			DiscordWebhookURL: discordServer.URL,
			SESSenderEmail:    "sender@test.com",
			RecipientEmail:    "recipient@test.com",
		},
	}

	err := app.handler(context.Background(), snsEvent(testMessage()))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !discordCalled {
		t.Error("expected Discord webhook to be called")
	}
	if !emailCalled {
		t.Error("expected SES SendEmail to be called")
	}
}

func TestHandler_OnlyDiscordConfigured(t *testing.T) {
	var discordCalled bool

	discordServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		discordCalled = true
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer discordServer.Close()

	sesClient := &mockSES{
		sendEmailFunc: func(ctx context.Context, params *ses.SendEmailInput, optFns ...func(*ses.Options)) (*ses.SendEmailOutput, error) {
			t.Error("SES should not be called when email not configured")
			return &ses.SendEmailOutput{}, nil
		},
	}

	app := &App{
		ses:  sesClient,
		http: discordServer.Client(),
		cfg: AppConfig{
			DiscordWebhookURL: discordServer.URL,
		},
	}

	err := app.handler(context.Background(), snsEvent(testMessage()))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !discordCalled {
		t.Error("expected Discord webhook to be called")
	}
}

func TestHandler_NeitherConfigured(t *testing.T) {
	app := &App{
		cfg: AppConfig{},
	}

	err := app.handler(context.Background(), snsEvent(testMessage()))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestHandler_DiscordHTTPError(t *testing.T) {
	discordServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer discordServer.Close()

	app := &App{
		http: discordServer.Client(),
		cfg: AppConfig{
			DiscordWebhookURL: discordServer.URL,
		},
	}

	// Should not return error — Discord failures are logged but don't fail Lambda
	err := app.handler(context.Background(), snsEvent(testMessage()))
	if err != nil {
		t.Fatalf("expected no error (Discord errors are logged, not returned), got: %v", err)
	}
}

func TestHandler_SESError(t *testing.T) {
	sesClient := &mockSES{
		sendEmailFunc: func(ctx context.Context, params *ses.SendEmailInput, optFns ...func(*ses.Options)) (*ses.SendEmailOutput, error) {
			return nil, fmt.Errorf("SES rate limit exceeded")
		},
	}

	app := &App{
		ses: sesClient,
		cfg: AppConfig{
			SESSenderEmail: "sender@test.com",
			RecipientEmail: "recipient@test.com",
		},
	}

	// Should not return error — email failures are logged but don't fail Lambda
	err := app.handler(context.Background(), snsEvent(testMessage()))
	if err != nil {
		t.Fatalf("expected no error (email errors are logged, not returned), got: %v", err)
	}
}

func TestHandler_InvalidSNSMessage(t *testing.T) {
	app := &App{
		cfg: AppConfig{},
	}

	badEvent := events.SNSEvent{
		Records: []events.SNSEventRecord{
			{
				SNS: events.SNSEntity{
					Message: "not valid json{{{",
				},
			},
		},
	}

	// Should not error — invalid messages are logged and skipped
	err := app.handler(context.Background(), badEvent)
	if err != nil {
		t.Fatalf("expected no error for invalid SNS message, got: %v", err)
	}
}
