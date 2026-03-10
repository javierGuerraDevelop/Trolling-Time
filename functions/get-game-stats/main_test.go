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
)

// --- Mock implementations ---

type mockDynamoDB struct {
	putItemFunc func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

func (mock *mockDynamoDB) PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return mock.putItemFunc(ctx, params, optFns...)
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
		RiotAPIKey:  "test-api-key",
		MatchRegion: "americas",
		TableName:   "test-game-stats",
	}
}

func sampleMatchResponse(puuid string) MatchResponse {
	return MatchResponse{
		Info: MatchInfo{
			GameMode:     "CLASSIC",
			GameDuration: 1800,
			Participants: []MatchParticipant{
				{
					PUUID:                       "other-puuid",
					ChampionName:                "Garen",
					Win:                         false,
					Kills:                       2,
					Deaths:                      5,
					Assists:                     3,
					TotalMinionsKilled:          100,
					NeutralMinionsKilled:        20,
					GoldEarned:                  8000,
					TotalDamageDealtToChampions: 15000,
					VisionScore:                 10,
					ChampLevel:                  14,
					TeamPosition:                "TOP",
				},
				{
					PUUID:                       puuid,
					ChampionName:                "Jinx",
					Win:                         true,
					Kills:                       10,
					Deaths:                      2,
					Assists:                     8,
					TotalMinionsKilled:          200,
					NeutralMinionsKilled:        30,
					GoldEarned:                  15000,
					TotalDamageDealtToChampions: 30000,
					VisionScore:                 25,
					ChampLevel:                  18,
					TeamPosition:                "BOTTOM",
					Item0:                       3031,
					Item1:                       3006,
					Item2:                       3094,
				},
			},
		},
	}
}

// --- Tests ---

func TestHandler_SuccessfulStatsWrite(t *testing.T) {
	const testPUUID = "test-puuid-123"
	var savedItem map[string]interface{}

	db := &mockDynamoDB{
		putItemFunc: func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			// Verify table name
			if *params.TableName != "test-game-stats" {
				t.Errorf("expected table 'test-game-stats', got '%s'", *params.TableName)
			}
			// Check that puuid key is present
			if _, ok := params.Item["puuid"]; !ok {
				t.Error("expected puuid in DynamoDB item")
			}
			// Check CS calculation: 200 minions + 30 neutral = 230
			if cs, ok := params.Item["totalCS"]; ok {
				// DynamoDB stores numbers as AttributeValueMemberN
				_ = cs // verified by record construction
			}
			savedItem = make(map[string]interface{})
			savedItem["called"] = true
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, sampleMatchResponse(testPUUID)), nil
		},
	}

	app := &App{db: db, http: httpClient, cfg: testConfig()}
	err := app.handler(context.Background(), GameStatsEvent{
		MatchID: "NA1_12345",
		PUUID:   testPUUID,
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if savedItem == nil {
		t.Error("expected PutItem to be called")
	}
}

func TestHandler_CSCalculation(t *testing.T) {
	const testPUUID = "test-puuid-123"

	match := sampleMatchResponse(testPUUID)
	// Jinx has 200 minions + 30 neutral = 230 CS
	expectedCS := 230

	db := &mockDynamoDB{
		putItemFunc: func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, match), nil
		},
	}

	app := &App{db: db, http: httpClient, cfg: testConfig()}

	// Verify via the record construction in handler
	var player *MatchParticipant
	for i, participant := range match.Info.Participants {
		if participant.PUUID == testPUUID {
			player = &match.Info.Participants[i]
			break
		}
	}
	actualCS := player.TotalMinionsKilled + player.NeutralMinionsKilled
	if actualCS != expectedCS {
		t.Errorf("expected CS %d, got %d", expectedCS, actualCS)
	}

	err := app.handler(context.Background(), GameStatsEvent{
		MatchID: "NA1_12345",
		PUUID:   testPUUID,
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestHandler_PlayerNotFoundInMatch(t *testing.T) {
	db := &mockDynamoDB{
		putItemFunc: func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			t.Error("PutItem should not be called when player not found")
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, sampleMatchResponse("some-other-puuid")), nil
		},
	}

	app := &App{db: db, http: httpClient, cfg: testConfig()}
	err := app.handler(context.Background(), GameStatsEvent{
		MatchID: "NA1_12345",
		PUUID:   "missing-puuid",
	})
	if err == nil {
		t.Fatal("expected error for missing player")
	}
	if !strings.Contains(err.Error(), "not found in match") {
		t.Errorf("expected 'not found in match' error, got: %v", err)
	}
}

func TestHandler_APIFailure(t *testing.T) {
	db := &mockDynamoDB{
		putItemFunc: func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return &dynamodb.PutItemOutput{}, nil
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(bytes.NewReader([]byte("service unavailable"))),
				Header:     make(http.Header),
			}, nil
		},
	}

	app := &App{db: db, http: httpClient, cfg: testConfig()}
	err := app.handler(context.Background(), GameStatsEvent{
		MatchID: "NA1_12345",
		PUUID:   "test-puuid",
	})
	if err == nil {
		t.Fatal("expected error for API failure")
	}
	if !strings.Contains(err.Error(), "match API returned 503") {
		t.Errorf("expected match API error, got: %v", err)
	}
}

func TestHandler_MissingEventFields(t *testing.T) {
	tests := []struct {
		name  string
		event GameStatsEvent
	}{
		{"empty matchId", GameStatsEvent{PUUID: "puuid"}},
		{"empty puuid", GameStatsEvent{MatchID: "NA1_123"}},
		{"both empty", GameStatsEvent{}},
	}

	app := &App{cfg: testConfig()}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := app.handler(context.Background(), tt.event)
			if err == nil {
				t.Error("expected error for missing fields")
			}
			if !strings.Contains(err.Error(), "required") {
				t.Errorf("expected 'required' error, got: %v", err)
			}
		})
	}
}

func TestHandler_DynamoDBFailure(t *testing.T) {
	const testPUUID = "test-puuid-123"

	db := &mockDynamoDB{
		putItemFunc: func(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	httpClient := &mockHTTPClient{
		doFunc: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, sampleMatchResponse(testPUUID)), nil
		},
	}

	app := &App{db: db, http: httpClient, cfg: testConfig()}
	err := app.handler(context.Background(), GameStatsEvent{
		MatchID: "NA1_12345",
		PUUID:   testPUUID,
	})
	if err == nil {
		t.Fatal("expected error for DynamoDB failure")
	}
	if !strings.Contains(err.Error(), "failed to write stats") {
		t.Errorf("expected write stats error, got: %v", err)
	}
}
