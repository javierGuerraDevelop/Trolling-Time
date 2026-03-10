package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/scheduler"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

// --- Mock implementations ---

type mockDynamoDB struct {
	getItemFunc    func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	putItemFunc    func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	scanFunc       func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
	updateItemFunc func(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

func (mock *mockDynamoDB) GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return mock.getItemFunc(ctx, params, optFns...)
}
func (mock *mockDynamoDB) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return mock.putItemFunc(ctx, params, optFns...)
}
func (mock *mockDynamoDB) Scan(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	return mock.scanFunc(ctx, params, optFns...)
}
func (mock *mockDynamoDB) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	return mock.updateItemFunc(ctx, params, optFns...)
}

type mockSNS struct {
	publishFunc func(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error)
}

func (mock *mockSNS) Publish(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error) {
	return mock.publishFunc(ctx, params, optFns...)
}

type mockScheduler struct {
	createScheduleFunc func(ctx context.Context, params *scheduler.CreateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error)
}

func (mock *mockScheduler) CreateSchedule(ctx context.Context, params *scheduler.CreateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error) {
	return mock.createScheduleFunc(ctx, params, optFns...)
}

type mockHTTPClient struct {
	doFunc func(req *http.Request) (*http.Response, error)
}

func (mock *mockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return mock.doFunc(req)
}

// --- Helpers ---

func jsonResponse(statusCode int, body interface{}) *http.Response {
	data, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(bytes.NewReader(data)),
		Header:     make(http.Header),
	}
}

func testConfig() AppConfig {
	return AppConfig{
		RiotAPIKey:         "test-api-key",
		RiotRegion:         "na1",
		PlayersTableName:   "test-players",
		GameStatsTableName: "test-game-stats",
		SNSTopicARN:        "arn:aws:sns:us-east-1:123456789:test-topic",
		GetGameStatsFnARN:  "arn:aws:lambda:us-east-1:123456789:function:test",
		SchedulerRoleARN:   "arn:aws:iam::123456789:role/test",
		MatchRegion:        "americas",
	}
}

func playerItems(players ...Player) []map[string]dbtypes.AttributeValue {
	items := make([]map[string]dbtypes.AttributeValue, len(players))
	for i, player := range players {
		item := map[string]dbtypes.AttributeValue{
			"playerId": &dbtypes.AttributeValueMemberS{Value: player.PlayerID},
			"gameName": &dbtypes.AttributeValueMemberS{Value: player.GameName},
			"tagLine":  &dbtypes.AttributeValueMemberS{Value: player.TagLine},
		}
		if player.PUUID != "" {
			item["puuid"] = &dbtypes.AttributeValueMemberS{Value: player.PUUID}
		}
		items[i] = item
	}
	return items
}

// --- Tests ---

func TestHandler_PlayerNotInGame(t *testing.T) {
	db := &mockDynamoDB{
		scanFunc: func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			return &dynamodb.ScanOutput{
				Items: playerItems(Player{PlayerID: "Test#NA1", GameName: "Test", TagLine: "NA1", PUUID: "puuid-12345678"}),
			}, nil
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "active-games") {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(bytes.NewReader(nil)),
					Header:     make(http.Header),
				}, nil
			}
			return nil, fmt.Errorf("unexpected request: %s", req.URL)
		},
	}

	app := &App{db: db, http: httpClient, cfg: testConfig()}
	err := app.handler(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestHandler_FirstDetection(t *testing.T) {
	var putItemCalled, publishCalled, scheduleCalled bool

	db := &mockDynamoDB{
		scanFunc: func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			return &dynamodb.ScanOutput{
				Items: playerItems(Player{PlayerID: "Test#NA1", GameName: "Test", TagLine: "NA1", PUUID: "puuid-12345678"}),
			}, nil
		},
		getItemFunc: func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: nil}, nil
		},
		putItemFunc: func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			putItemCalled = true
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	spectatorResp := SpectatorResponse{
		GameID:     12345,
		PlatformID: "NA1",
		GameMode:   "CLASSIC",
		Participants: []Participant{
			{PUUID: "puuid-12345678", ChampionID: 99},
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "active-games") {
				return jsonResponse(http.StatusOK, spectatorResp), nil
			}
			return nil, fmt.Errorf("unexpected request: %s", req.URL)
		},
	}

	snsClient := &mockSNS{
		publishFunc: func(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error) {
			publishCalled = true
			var msg NotificationMessage
			if err := json.Unmarshal([]byte(*params.Message), &msg); err != nil {
				t.Errorf("failed to unmarshal SNS message: %v", err)
			}
			if msg.PlayerName != "Test" {
				t.Errorf("expected playerName 'Test', got '%s'", msg.PlayerName)
			}
			if msg.ChampionID != 99 {
				t.Errorf("expected championId 99, got %d", msg.ChampionID)
			}
			return &sns.PublishOutput{}, nil
		},
	}

	schedulerClient := &mockScheduler{
		createScheduleFunc: func(ctx context.Context, params *scheduler.CreateScheduleInput, optFns ...func(*scheduler.Options)) (*scheduler.CreateScheduleOutput, error) {
			scheduleCalled = true
			if !strings.Contains(*params.Name, "puuid-12") {
				t.Errorf("schedule name should contain puuid prefix, got: %s", *params.Name)
			}
			return &scheduler.CreateScheduleOutput{}, nil
		},
	}

	app := &App{db: db, sns: snsClient, scheduler: schedulerClient, http: httpClient, cfg: testConfig()}
	err := app.handler(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !putItemCalled {
		t.Error("expected PutItem to be called")
	}
	if !publishCalled {
		t.Error("expected SNS Publish to be called")
	}
	if !scheduleCalled {
		t.Error("expected CreateSchedule to be called")
	}
}

func TestHandler_AlreadyTracked(t *testing.T) {
	var putItemCalled bool

	db := &mockDynamoDB{
		scanFunc: func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			return &dynamodb.ScanOutput{
				Items: playerItems(Player{PlayerID: "Test#NA1", GameName: "Test", TagLine: "NA1", PUUID: "puuid-12345678"}),
			}, nil
		},
		getItemFunc: func(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{
				Item: map[string]dbtypes.AttributeValue{
					"matchId": &dbtypes.AttributeValueMemberS{Value: "NA1_12345"},
				},
			}, nil
		},
		putItemFunc: func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			putItemCalled = true
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	spectatorResp := SpectatorResponse{
		GameID:     12345,
		PlatformID: "NA1",
		GameMode:   "CLASSIC",
		Participants: []Participant{
			{PUUID: "puuid-12345678", ChampionID: 99},
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "active-games") {
				return jsonResponse(http.StatusOK, spectatorResp), nil
			}
			return nil, fmt.Errorf("unexpected request: %s", req.URL)
		},
	}

	app := &App{db: db, http: httpClient, cfg: testConfig()}
	err := app.handler(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if putItemCalled {
		t.Error("PutItem should not be called for already tracked game")
	}
}

func TestHandler_RiotAPIError(t *testing.T) {
	db := &mockDynamoDB{
		scanFunc: func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			// Player without cached PUUID to trigger getPUUID call
			return &dynamodb.ScanOutput{
				Items: playerItems(Player{PlayerID: "Test#NA1", GameName: "Test", TagLine: "NA1"}),
			}, nil
		},
		updateItemFunc: func(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "by-riot-id") {
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Body:       io.NopCloser(bytes.NewReader([]byte("internal error"))),
					Header:     make(http.Header),
				}, nil
			}
			return nil, fmt.Errorf("unexpected request: %s", req.URL)
		},
	}

	app := &App{db: db, http: httpClient, cfg: testConfig()}
	err := app.handler(context.Background())
	if err == nil {
		t.Fatal("expected error for Riot API failure")
	}
	if !strings.Contains(err.Error(), "all players failed") {
		t.Errorf("expected 'all players failed' error, got: %v", err)
	}
}

func TestHandler_NoPlayersConfigured(t *testing.T) {
	db := &mockDynamoDB{
		scanFunc: func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			return &dynamodb.ScanOutput{Items: nil}, nil
		},
	}

	app := &App{db: db, cfg: testConfig()}
	err := app.handler(context.Background())
	if err != nil {
		t.Fatalf("expected no error for empty player list, got: %v", err)
	}
}

func TestHandler_PUUIDCaching(t *testing.T) {
	var updateItemCalled bool

	db := &mockDynamoDB{
		scanFunc: func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			return &dynamodb.ScanOutput{
				Items: playerItems(Player{PlayerID: "Test#NA1", GameName: "Test", TagLine: "NA1"}),
			}, nil
		},
		updateItemFunc: func(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
			updateItemCalled = true
			puuidVal := params.ExpressionAttributeValues[":p"].(*dbtypes.AttributeValueMemberS).Value
			if puuidVal != "resolved-puuid" {
				t.Errorf("expected cached puuid 'resolved-puuid', got '%s'", puuidVal)
			}
			return &dynamodb.UpdateItemOutput{}, nil
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "by-riot-id") {
				return jsonResponse(http.StatusOK, RiotAccountResponse{PUUID: "resolved-puuid"}), nil
			}
			if strings.Contains(req.URL.Path, "active-games") {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(bytes.NewReader(nil)),
					Header:     make(http.Header),
				}, nil
			}
			return nil, fmt.Errorf("unexpected request: %s", req.URL)
		},
	}

	app := &App{db: db, http: httpClient, cfg: testConfig()}
	err := app.handler(context.Background())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !updateItemCalled {
		t.Error("expected UpdateItem to be called to cache PUUID")
	}
}

func TestHandler_PartialPlayerFailure(t *testing.T) {
	callCount := 0

	db := &mockDynamoDB{
		scanFunc: func(ctx context.Context, params *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
			return &dynamodb.ScanOutput{
				Items: playerItems(
					Player{PlayerID: "Good#NA1", GameName: "Good", TagLine: "NA1", PUUID: "good-puuid-12345"},
					Player{PlayerID: "Bad#NA1", GameName: "Bad", TagLine: "NA1", PUUID: "bad-puuid-12345678"},
				),
			}, nil
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			callCount++
			if strings.Contains(req.URL.Path, "good-puuid") {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(bytes.NewReader(nil)),
					Header:     make(http.Header),
				}, nil
			}
			// Bad player: spectator API returns error
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(bytes.NewReader([]byte("server error"))),
				Header:     make(http.Header),
			}, nil
		},
	}

	app := &App{db: db, http: httpClient, cfg: testConfig()}
	err := app.handler(context.Background())
	// Should succeed because not ALL players failed
	if err != nil {
		t.Fatalf("expected no error when only some players fail, got: %v", err)
	}
}
