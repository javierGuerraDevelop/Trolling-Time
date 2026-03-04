package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
)

type Request struct {
	PlayerName string `json:"playerName"`
	TagLine    string `json:"tagLine"`
}

type Response struct {
	PUUID string `json:"puuid"`
}

type RiotAccountResponse struct {
	PUUID    string `json:"puuid"`
	GameName string `json:"gameName"`
	TagLine  string `json:"tagLine"`
}

func handler(ctx context.Context, req Request) (Response, error) {
	apiKey := os.Getenv("RIOT_API_KEY")
	if apiKey == "" {
		return Response{}, fmt.Errorf("RIOT_API_KEY environment variable not set")
	}

	url := fmt.Sprintf("https://americas.api.riotgames.com/riot/account/v1/accounts/by-riot-id/%s/%s", req.PlayerName, req.TagLine)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Response{}, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("X-Riot-Token", apiKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("calling Riot API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("Riot API returned status %d", resp.StatusCode)
	}

	var riotResp RiotAccountResponse
	if err := json.NewDecoder(resp.Body).Decode(&riotResp); err != nil {
		return Response{}, fmt.Errorf("decoding response: %w", err)
	}

	log.Printf("Resolved %s#%s to PUUID %s", req.PlayerName, req.TagLine, riotResp.PUUID)

	return Response{PUUID: riotResp.PUUID}, nil
}

func main() {
	lambda.Start(handler)
}
