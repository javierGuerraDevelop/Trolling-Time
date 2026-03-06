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
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type GameStatsEvent struct {
	MatchID string `json:"matchId"`
	PUUID   string `json:"puuid"`
}

type MatchResponse struct {
	Info MatchInfo `json:"info"`
}

type MatchInfo struct {
	GameMode     string             `json:"gameMode"`
	GameDuration int64              `json:"gameDuration"`
	Participants []MatchParticipant `json:"participants"`
}

type MatchParticipant struct {
	PUUID                      string `json:"puuid"`
	ChampionName               string `json:"championName"`
	Win                        bool   `json:"win"`
	Kills                      int    `json:"kills"`
	Deaths                     int    `json:"deaths"`
	Assists                    int    `json:"assists"`
	TotalMinionsKilled         int    `json:"totalMinionsKilled"`
	NeutralMinionsKilled       int    `json:"neutralMinionsKilled"`
	GoldEarned                 int    `json:"goldEarned"`
	TotalDamageDealtToChampions int   `json:"totalDamageDealtToChampions"`
	VisionScore                int    `json:"visionScore"`
	ChampLevel                 int    `json:"champLevel"`
	TeamPosition               string `json:"teamPosition"`
	Item0                      int    `json:"item0"`
	Item1                      int    `json:"item1"`
	Item2                      int    `json:"item2"`
	Item3                      int    `json:"item3"`
	Item4                      int    `json:"item4"`
	Item5                      int    `json:"item5"`
	Item6                      int    `json:"item6"`
}

type GameStatsRecord struct {
	MatchID                     string `dynamodbav:"matchId"`
	ChampionName                string `dynamodbav:"championName"`
	GameMode                    string `dynamodbav:"gameMode"`
	Win                         bool   `dynamodbav:"win"`
	Kills                       int    `dynamodbav:"kills"`
	Deaths                      int    `dynamodbav:"deaths"`
	Assists                     int    `dynamodbav:"assists"`
	TotalCS                     int    `dynamodbav:"totalCS"`
	GoldEarned                  int    `dynamodbav:"goldEarned"`
	TotalDamageDealtToChampions int    `dynamodbav:"totalDamageDealtToChampions"`
	VisionScore                 int    `dynamodbav:"visionScore"`
	ChampLevel                  int    `dynamodbav:"champLevel"`
	TeamPosition                string `dynamodbav:"teamPosition"`
	GameDuration                int64  `dynamodbav:"gameDuration"`
	Item0                       int    `dynamodbav:"item0"`
	Item1                       int    `dynamodbav:"item1"`
	Item2                       int    `dynamodbav:"item2"`
	Item3                       int    `dynamodbav:"item3"`
	Item4                       int    `dynamodbav:"item4"`
	Item5                       int    `dynamodbav:"item5"`
	Item6                       int    `dynamodbav:"item6"`
}

func handler(ctx context.Context, event GameStatsEvent) error {
	apiKey := os.Getenv("RIOT_API_KEY")
	matchRegion := os.Getenv("MATCH_REGION")
	tableName := os.Getenv("DYNAMO_TABLE_NAME")

	if apiKey == "" || matchRegion == "" || tableName == "" {
		return fmt.Errorf("RIOT_API_KEY, MATCH_REGION, and DYNAMO_TABLE_NAME must be set")
	}
	if event.MatchID == "" || event.PUUID == "" {
		return fmt.Errorf("matchId and puuid are required")
	}

	log.Printf("Fetching match stats for %s (player %s)", event.MatchID, event.PUUID)

	match, err := getMatchDetails(ctx, apiKey, matchRegion, event.MatchID)
	if err != nil {
		return fmt.Errorf("failed to get match details: %w", err)
	}

	var player *MatchParticipant
	for i, p := range match.Info.Participants {
		if p.PUUID == event.PUUID {
			player = &match.Info.Participants[i]
			break
		}
	}
	if player == nil {
		return fmt.Errorf("player %s not found in match %s", event.PUUID, event.MatchID)
	}

	record := GameStatsRecord{
		MatchID:                     event.MatchID,
		ChampionName:                player.ChampionName,
		GameMode:                    match.Info.GameMode,
		Win:                         player.Win,
		Kills:                       player.Kills,
		Deaths:                      player.Deaths,
		Assists:                     player.Assists,
		TotalCS:                     player.TotalMinionsKilled + player.NeutralMinionsKilled,
		GoldEarned:                  player.GoldEarned,
		TotalDamageDealtToChampions: player.TotalDamageDealtToChampions,
		VisionScore:                 player.VisionScore,
		ChampLevel:                  player.ChampLevel,
		TeamPosition:                player.TeamPosition,
		GameDuration:                match.Info.GameDuration,
		Item0:                       player.Item0,
		Item1:                       player.Item1,
		Item2:                       player.Item2,
		Item3:                       player.Item3,
		Item4:                       player.Item4,
		Item5:                       player.Item5,
		Item6:                       player.Item6,
	}

	if err := writeStats(ctx, tableName, record); err != nil {
		return fmt.Errorf("failed to write stats to DynamoDB: %w", err)
	}

	log.Printf("Successfully saved stats for match %s", event.MatchID)
	return nil
}

func getMatchDetails(ctx context.Context, apiKey, matchRegion, matchID string) (*MatchResponse, error) {
	url := fmt.Sprintf("https://%s.api.riotgames.com/lol/match/v5/matches/%s", matchRegion, matchID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Riot-Token", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("match API returned %d: %s", resp.StatusCode, body)
	}

	var match MatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&match); err != nil {
		return nil, err
	}
	return &match, nil
}

func writeStats(ctx context.Context, tableName string, record GameStatsRecord) error {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("failed to marshal record: %w", err)
	}

	client := dynamodb.NewFromConfig(cfg)
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &tableName,
		Item:      item,
	})
	return err
}

func main() {
	lambda.Start(handler)
}
