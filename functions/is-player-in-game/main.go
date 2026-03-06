package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/ses/types"
)

type Request struct {
	PlayerName     string `json:"playerName"`
	TagLine        string `json:"tagLine"`
	RecipientEmail string `json:"recipientEmail"`
}

type Response struct {
	InGame     bool   `json:"inGame"`
	GameMode   string `json:"gameMode,omitempty"`
	ChampionID int64  `json:"championId,omitempty"`
}

type RiotAccountResponse struct {
	PUUID string `json:"puuid"`
}

type SpectatorResponse struct {
	GameMode      string        `json:"gameMode"`
	GameStartTime int64         `json:"gameStartTime"`
	Participants  []Participant `json:"participants"`
}

type Participant struct {
	PUUID      string `json:"puuid"`
	ChampionID int64  `json:"championId"`
}

func handler(ctx context.Context, req Request) (Response, error) {
	apiKey := os.Getenv("RIOT_API_KEY")
	region := os.Getenv("RIOT_REGION")
	if region == "" {
		region = "na1"
	}

	if apiKey == "" {
		return Response{}, fmt.Errorf("missing riot apiKey")
	}

	if req.PlayerName == "" {
		req.PlayerName = os.Getenv("PLAYER_NAME")
	}
	if req.TagLine == "" {
		req.TagLine = os.Getenv("TAG_LINE")
	}
	if req.RecipientEmail == "" {
		req.RecipientEmail = os.Getenv("RECIPIENT_EMAIL")
	}

	if req.PlayerName == "" || req.TagLine == "" || req.RecipientEmail == "" {
		return Response{}, fmt.Errorf("playerName, tagLine, and email are required")
	}

	puuid, err := getPUUID(ctx, apiKey, req.PlayerName, req.TagLine)
	if err != nil {
		return Response{}, fmt.Errorf("failed to get PUUID: %w", err)
	}

	spectator, err := getActiveGame(apiKey, region, puuid)
	if err != nil {
		return Response{}, fmt.Errorf("failed to check active game: %w", err)
	}
	if spectator == nil {
		return Response{InGame: false}, nil
	}

	var championID int64
	for _, p := range spectator.Participants {
		if p.PUUID == puuid {
			championID = p.ChampionID
			break
		}
	}

	if req.RecipientEmail != "" {
		if err := sendEmail(ctx, req.RecipientEmail, req.PlayerName, spectator.GameMode, championID, spectator.GameStartTime); err != nil {
			return Response{}, fmt.Errorf("failed to send email: %w", err)
		}
	}

	return Response{
		InGame:     true,
		GameMode:   spectator.GameMode,
		ChampionID: championID,
	}, nil
}

func getPUUID(ctx context.Context, apiKey, name, tag string) (string, error) {
	url := fmt.Sprintf("https://americas.api.riotgames.com/riot/account/v1/accounts/by-riot-id/%s/%s", name, tag)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("X-Riot-Token", apiKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("riot account API returned %d: %s", resp.StatusCode, body)
	}

	var account RiotAccountResponse
	if err := json.NewDecoder(resp.Body).Decode(&account); err != nil {
		return "", err
	}

	log.Printf("Resolved %s#%s to PUUID %s", name, tag, account.PUUID)
	return account.PUUID, nil
}

func getActiveGame(apiKey, region, puuid string) (*SpectatorResponse, error) {
	url := fmt.Sprintf("https://%s.api.riotgames.com/lol/spectator/v5/active-games/by-summoner/%s", region, puuid)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Riot-Token", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("spectator API returned %d: %s", resp.StatusCode, body)
	}

	var spectator SpectatorResponse
	if err := json.NewDecoder(resp.Body).Decode(&spectator); err != nil {
		return nil, err
	}
	return &spectator, nil
}

func sendEmail(ctx context.Context, recipient, playerName, gameMode string, championID, gameStartTime int64) error {
	senderEmail := os.Getenv("SES_SENDER_EMAIL")
	if senderEmail == "" {
		return fmt.Errorf("SES_SENDER_EMAIL not set")
	}

	subject := fmt.Sprintf("%s is in a game!", playerName)
	body := fmt.Sprintf("%s is currently in a %s game (Champion ID: %d, Start Time: %d)",
		playerName, gameMode, championID, gameStartTime)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	client := ses.NewFromConfig(cfg)
	_, err = client.SendEmail(ctx, &ses.SendEmailInput{
		Source: &senderEmail,
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
