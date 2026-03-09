/*
IsPlayerInGameFunction polls the Riot Spectator API every 5 minutes to check
if a player is in an active game. On first detection it writes the match to
DynamoDB (for deduplication), publishes a notification to SNS, and
schedules a one-time EventBridge event to collect post-game stats.
*/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

type Request struct {
	PlayerName string `json:"playerName"`
	TagLine    string `json:"tagLine"`
}

type NotificationMessage struct {
	PlayerName    string `json:"playerName"`
	GameMode      string `json:"gameMode"`
	ChampionID    int64  `json:"championId"`
	GameStartTime int64  `json:"gameStartTime"`
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
	GameID        int64         `json:"gameId"`
	PlatformID    string        `json:"platformId"`
	GameMode      string        `json:"gameMode"`
	GameStartTime int64         `json:"gameStartTime"`
	Participants  []Participant `json:"participants"`
}

type Participant struct {
	PUUID      string `json:"puuid"`
	ChampionID int64  `json:"championId"`
}

/*
handler resolves the player's PUUID, checks for an active game, deduplicates
via DynamoDB, sends a notification, and schedules post-game stats collection.
*/
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

	if req.PlayerName == "" || req.TagLine == "" {
		return Response{}, fmt.Errorf("playerName and tagLine are required")
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

	matchID := fmt.Sprintf("%s_%d", spectator.PlatformID, spectator.GameID)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return Response{}, fmt.Errorf("failed to load AWS config: %w", err)
	}

	tableName := os.Getenv("DYNAMO_TABLE_NAME")
	dbClient := dynamodb.NewFromConfig(cfg)

	tracked, err := gameAlreadyTracked(ctx, dbClient, tableName, matchID)
	if err != nil {
		return Response{}, fmt.Errorf("failed to check DynamoDB: %w", err)
	}
	if tracked {
		log.Printf("Game %s already tracked, skipping", matchID)
		return Response{InGame: true, GameMode: spectator.GameMode}, nil
	}

	if err := writeGameRecord(ctx, dbClient, tableName, matchID); err != nil {
		return Response{}, fmt.Errorf("failed to write game record: %w", err)
	}

	var championID int64
	for _, p := range spectator.Participants {
		if p.PUUID == puuid {
			championID = p.ChampionID
			break
		}
	}

	if err := publishNotification(ctx, cfg, req.PlayerName, spectator.GameMode, championID, spectator.GameStartTime); err != nil {
		return Response{}, fmt.Errorf("failed to publish notification: %w", err)
	}

	schedulerClient := scheduler.NewFromConfig(cfg)
	if err := createOneTimeSchedule(ctx, schedulerClient, matchID, puuid); err != nil {
		log.Printf("WARNING: failed to create stats schedule: %v", err)
	}

	return Response{
		InGame:     true,
		GameMode:   spectator.GameMode,
		ChampionID: championID,
	}, nil
}

// gameAlreadyTracked checks if a matchId already exists in DynamoDB to prevent duplicate notifications.
func gameAlreadyTracked(ctx context.Context, client *dynamodb.Client, tableName, matchID string) (bool, error) {
	result, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &tableName,
		Key: map[string]dbtypes.AttributeValue{
			"matchId": &dbtypes.AttributeValueMemberS{Value: matchID},
		},
		ProjectionExpression: aws.String("matchId"),
	})
	if err != nil {
		return false, err
	}
	return result.Item != nil, nil
}

// writeGameRecord creates a minimal DynamoDB record with just the matchId key.
// Stats fields are populated later by GetGameStatsFunction.
func writeGameRecord(ctx context.Context, client *dynamodb.Client, tableName, matchID string) error {
	_, err := client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &tableName,
		Item: map[string]dbtypes.AttributeValue{
			"matchId": &dbtypes.AttributeValueMemberS{Value: matchID},
		},
	})
	return err
}

/*
createOneTimeSchedule creates an EventBridge Scheduler one-time schedule that
invokes GetGameStatsFunction in 1 hour. The schedule auto-deletes after execution.
*/
func createOneTimeSchedule(ctx context.Context, client *scheduler.Client, matchID, puuid string) error {
	functionArn := os.Getenv("GET_GAME_STATS_FUNCTION_ARN")
	roleArn := os.Getenv("SCHEDULER_ROLE_ARN")
	matchRegion := os.Getenv("MATCH_REGION")

	if functionArn == "" || roleArn == "" || matchRegion == "" {
		return fmt.Errorf("GET_GAME_STATS_FUNCTION_ARN, SCHEDULER_ROLE_ARN, and MATCH_REGION must be set")
	}

	scheduleName := fmt.Sprintf("game-stats-%s", matchID)
	scheduleTime := time.Now().Add(1 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	scheduleExpr := fmt.Sprintf("at(%s)", scheduleTime)

	payload := fmt.Sprintf(`{"matchId":"%s","puuid":"%s"}`, matchID, puuid)

	deleteAction := schedulertypes.ActionAfterCompletionDelete

	_, err := client.CreateSchedule(ctx, &scheduler.CreateScheduleInput{
		Name:                       &scheduleName,
		ScheduleExpression:         &scheduleExpr,
		ScheduleExpressionTimezone: aws.String("UTC"),
		ActionAfterCompletion:      deleteAction,
		FlexibleTimeWindow:         &schedulertypes.FlexibleTimeWindow{Mode: schedulertypes.FlexibleTimeWindowModeOff},
		Target: &schedulertypes.Target{
			Arn:     &functionArn,
			RoleArn: &roleArn,
			Input:   &payload,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create schedule %s: %w", scheduleName, err)
	}

	log.Printf("Created one-time schedule %s for %s", scheduleName, scheduleTime)
	return nil
}

// getPUUID resolves a Riot ID (name#tag) to a PUUID via the Riot Account API.
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

// getActiveGame calls the Spectator V5 API. Returns nil if the player is not in a game.
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

// publishNotification publishes a game detection message to SNS for fan-out to notification channels.
func publishNotification(ctx context.Context, cfg aws.Config, playerName, gameMode string, championID, gameStartTime int64) error {
	topicArn := os.Getenv("SNS_TOPIC_ARN")
	if topicArn == "" {
		return fmt.Errorf("SNS_TOPIC_ARN not set")
	}

	msg := NotificationMessage{
		PlayerName:    playerName,
		GameMode:      gameMode,
		ChampionID:    championID,
		GameStartTime: gameStartTime,
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	client := sns.NewFromConfig(cfg)
	message := string(payload)
	_, err = client.Publish(ctx, &sns.PublishInput{
		TopicArn: &topicArn,
		Message:  &message,
	})
	return err
}

func main() {
	lambda.Start(handler)
}
