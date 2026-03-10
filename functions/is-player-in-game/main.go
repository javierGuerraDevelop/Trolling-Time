/*
IsPlayerInGameFunction polls the Riot Spectator API every 5 minutes to check
if tracked players are in an active game. On first detection it writes the match to
DynamoDB (for deduplication), publishes a notification to SNS, and
schedules a one-time EventBridge event to collect post-game stats.
*/
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	schedulertypes "github.com/aws/aws-sdk-go-v2/service/scheduler/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

// DynamoDBAPI is the interface for DynamoDB operations used by this function.
type DynamoDBAPI interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// SNSAPI is the interface for SNS operations used by this function.
type SNSAPI interface {
	Publish(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error)
}

// SchedulerAPI is the interface for EventBridge Scheduler operations used by this function.
type SchedulerAPI interface {
	CreateSchedule(ctx context.Context, params *scheduler.CreateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error)
}

// HTTPClient is the interface for making HTTP requests.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// AppConfig holds configuration values loaded from environment variables.
type AppConfig struct {
	RiotAPIKey         string
	RiotRegion         string
	PlayersTableName   string
	GameStatsTableName string
	SNSTopicARN        string
	GetGameStatsFnARN  string
	SchedulerRoleARN   string
	MatchRegion        string
}

// App holds the dependencies for the Lambda function.
type App struct {
	db        DynamoDBAPI
	sns       SNSAPI
	scheduler SchedulerAPI
	http      HTTPClient
	cfg       AppConfig
}

// Player represents a tracked player from the PlayersTable.
type Player struct {
	PlayerID string `dynamodbav:"playerId"`
	GameName string `dynamodbav:"gameName"`
	TagLine  string `dynamodbav:"tagLine"`
	PUUID    string `dynamodbav:"puuid,omitempty"`
}

type NotificationMessage struct {
	PlayerName    string `json:"playerName"`
	GameMode      string `json:"gameMode"`
	ChampionID    int64  `json:"championId"`
	GameStartTime int64  `json:"gameStartTime"`
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

func loadConfig() (AppConfig, error) {
	cfg := AppConfig{
		RiotAPIKey:         os.Getenv("RIOT_API_KEY"),
		RiotRegion:         os.Getenv("RIOT_REGION"),
		PlayersTableName:   os.Getenv("PLAYERS_TABLE_NAME"),
		GameStatsTableName: os.Getenv("DYNAMO_TABLE_NAME"),
		SNSTopicARN:        os.Getenv("SNS_TOPIC_ARN"),
		GetGameStatsFnARN:  os.Getenv("GET_GAME_STATS_FUNCTION_ARN"),
		SchedulerRoleARN:   os.Getenv("SCHEDULER_ROLE_ARN"),
		MatchRegion:        os.Getenv("MATCH_REGION"),
	}
	if cfg.RiotRegion == "" {
		cfg.RiotRegion = "na1"
	}
	if cfg.RiotAPIKey == "" {
		return cfg, fmt.Errorf("RIOT_API_KEY must be set")
	}
	if cfg.PlayersTableName == "" {
		return cfg, fmt.Errorf("PLAYERS_TABLE_NAME must be set")
	}
	if cfg.GameStatsTableName == "" {
		return cfg, fmt.Errorf("DYNAMO_TABLE_NAME must be set")
	}
	if cfg.SNSTopicARN == "" {
		return cfg, fmt.Errorf("SNS_TOPIC_ARN must be set")
	}
	return cfg, nil
}

/*
handler scans the PlayersTable and processes each tracked player:
resolve PUUID, check for active game, deduplicate, notify, and schedule stats.
Errors per player are logged but don't abort the loop.
*/
func (app *App) handler(ctx context.Context) error {
	players, err := app.getPlayers(ctx)
	if err != nil {
		return fmt.Errorf("failed to get players: %w", err)
	}
	if len(players) == 0 {
		log.Println("No players configured in PlayersTable")
		return nil
	}

	var errs []error
	for _, player := range players {
		if err := app.processPlayer(ctx, player); err != nil {
			log.Printf("ERROR processing %s: %v", player.PlayerID, err)
			errs = append(errs, fmt.Errorf("%s: %w", player.PlayerID, err))
		}
	}
	if len(errs) == len(players) {
		return fmt.Errorf("all players failed: %w", errors.Join(errs...))
	}
	return nil
}

// getPlayers scans the PlayersTable to retrieve all tracked players.
func (app *App) getPlayers(ctx context.Context) ([]Player, error) {
	result, err := app.db.Scan(ctx, &dynamodb.ScanInput{
		TableName: &app.cfg.PlayersTableName,
	})
	if err != nil {
		return nil, err
	}

	var players []Player
	if err := attributevalue.UnmarshalListOfMaps(result.Items, &players); err != nil {
		return nil, fmt.Errorf("failed to unmarshal players: %w", err)
	}
	return players, nil
}

// processPlayer checks if a single player is in an active game and handles notification.
func (app *App) processPlayer(ctx context.Context, player Player) error {
	puuid := player.PUUID
	if puuid == "" {
		var err error
		puuid, err = app.getPUUID(ctx, player.GameName, player.TagLine)
		if err != nil {
			return fmt.Errorf("failed to get PUUID: %w", err)
		}
		if err := app.cachePUUID(ctx, player.PlayerID, puuid); err != nil {
			log.Printf("WARNING: failed to cache PUUID for %s: %v", player.PlayerID, err)
		}
	}

	spectator, err := app.getActiveGame(ctx, puuid)
	if err != nil {
		return fmt.Errorf("failed to check active game: %w", err)
	}
	if spectator == nil {
		return nil
	}

	matchID := fmt.Sprintf("%s_%d", spectator.PlatformID, spectator.GameID)

	tracked, err := app.gameAlreadyTracked(ctx, matchID, puuid)
	if err != nil {
		return fmt.Errorf("failed to check DynamoDB: %w", err)
	}
	if tracked {
		log.Printf("Game %s already tracked for %s, skipping", matchID, player.PlayerID)
		return nil
	}

	if err := app.writeGameRecord(ctx, matchID, puuid); err != nil {
		return fmt.Errorf("failed to write game record: %w", err)
	}

	var championID int64
	for _, participant := range spectator.Participants {
		if participant.PUUID == puuid {
			championID = participant.ChampionID
			break
		}
	}

	if err := app.publishNotification(ctx, player.GameName, spectator.GameMode, championID, spectator.GameStartTime); err != nil {
		return fmt.Errorf("failed to publish notification: %w", err)
	}

	puuidPrefix := puuid
	if len(puuidPrefix) > 8 {
		puuidPrefix = puuidPrefix[:8]
	}
	scheduleName := fmt.Sprintf("game-stats-%s-%s", matchID, puuidPrefix)
	if err := app.createOneTimeSchedule(ctx, scheduleName, matchID, puuid); err != nil {
		log.Printf("WARNING: failed to create stats schedule: %v", err)
	}

	return nil
}

// cachePUUID updates the player's record in PlayersTable with their resolved PUUID.
func (app *App) cachePUUID(ctx context.Context, playerID, puuid string) error {
	_, err := app.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &app.cfg.PlayersTableName,
		Key: map[string]dbtypes.AttributeValue{
			"playerId": &dbtypes.AttributeValueMemberS{Value: playerID},
		},
		UpdateExpression: aws.String("SET puuid = :p"),
		ExpressionAttributeValues: map[string]dbtypes.AttributeValue{
			":p": &dbtypes.AttributeValueMemberS{Value: puuid},
		},
	})
	return err
}

// gameAlreadyTracked checks if a matchId+puuid composite key exists in DynamoDB.
func (app *App) gameAlreadyTracked(ctx context.Context, matchID, puuid string) (bool, error) {
	result, err := app.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &app.cfg.GameStatsTableName,
		Key: map[string]dbtypes.AttributeValue{
			"matchId": &dbtypes.AttributeValueMemberS{Value: matchID},
			"puuid":   &dbtypes.AttributeValueMemberS{Value: puuid},
		},
		ProjectionExpression: aws.String("matchId"),
	})
	if err != nil {
		return false, err
	}
	return result.Item != nil, nil
}

// writeGameRecord creates a minimal DynamoDB record with the matchId+puuid composite key.
func (app *App) writeGameRecord(ctx context.Context, matchID, puuid string) error {
	_, err := app.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &app.cfg.GameStatsTableName,
		Item: map[string]dbtypes.AttributeValue{
			"matchId": &dbtypes.AttributeValueMemberS{Value: matchID},
			"puuid":   &dbtypes.AttributeValueMemberS{Value: puuid},
		},
	})
	return err
}

/*
createOneTimeSchedule creates an EventBridge Scheduler one-time schedule that
invokes GetGameStatsFunction in 1 hour. The schedule auto-deletes after execution.
*/
func (app *App) createOneTimeSchedule(ctx context.Context, scheduleName, matchID, puuid string) error {
	if app.cfg.GetGameStatsFnARN == "" || app.cfg.SchedulerRoleARN == "" || app.cfg.MatchRegion == "" {
		return fmt.Errorf("GET_GAME_STATS_FUNCTION_ARN, SCHEDULER_ROLE_ARN, and MATCH_REGION must be set")
	}

	scheduleTime := time.Now().Add(1 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	scheduleExpr := fmt.Sprintf("at(%s)", scheduleTime)
	payload := fmt.Sprintf(`{"matchId":"%s","puuid":"%s"}`, matchID, puuid)
	deleteAction := schedulertypes.ActionAfterCompletionDelete

	_, err := app.scheduler.CreateSchedule(ctx, &scheduler.CreateScheduleInput{
		Name:                       &scheduleName,
		ScheduleExpression:         &scheduleExpr,
		ScheduleExpressionTimezone: aws.String("UTC"),
		ActionAfterCompletion:      deleteAction,
		FlexibleTimeWindow:         &schedulertypes.FlexibleTimeWindow{Mode: schedulertypes.FlexibleTimeWindowModeOff},
		Target: &schedulertypes.Target{
			Arn:     &app.cfg.GetGameStatsFnARN,
			RoleArn: &app.cfg.SchedulerRoleARN,
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
func (app *App) getPUUID(ctx context.Context, name, tag string) (string, error) {
	url := fmt.Sprintf("https://americas.api.riotgames.com/riot/account/v1/accounts/by-riot-id/%s/%s", name, tag)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("X-Riot-Token", app.cfg.RiotAPIKey)

	resp, err := app.http.Do(httpReq)
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
func (app *App) getActiveGame(ctx context.Context, puuid string) (*SpectatorResponse, error) {
	url := fmt.Sprintf("https://%s.api.riotgames.com/lol/spectator/v5/active-games/by-summoner/%s", app.cfg.RiotRegion, puuid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Riot-Token", app.cfg.RiotAPIKey)

	resp, err := app.http.Do(req)
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
func (app *App) publishNotification(ctx context.Context, playerName, gameMode string, championID, gameStartTime int64) error {
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

	message := string(payload)
	_, err = app.sns.Publish(ctx, &sns.PublishInput{
		TopicArn: &app.cfg.SNSTopicARN,
		Message:  &message,
	})
	return err
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	app := &App{
		db:        dynamodb.NewFromConfig(awsCfg),
		sns:       sns.NewFromConfig(awsCfg),
		scheduler: scheduler.NewFromConfig(awsCfg),
		http:      http.DefaultClient,
		cfg:       cfg,
	}

	lambda.Start(app.handler)
}
