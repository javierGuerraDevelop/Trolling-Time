# Trolling Time

Serverless app that monitors a League of Legends player and sends notifications (Discord, email) when they start a game, then collects post-game stats.

## Functions

### IsPlayerInGameFunction
Runs every 5 minutes via EventBridge. Checks the Riot Spectator API for an active game. On first detection, writes the match ID to DynamoDB, publishes a notification to SNS, and schedules a one-time stats collection 1 hour later. Duplicate detections are skipped using the DynamoDB record.

### NotifyWebhooksFunction
Triggered by SNS. Dispatches game detection notifications to all configured channels (Discord webhook, SES email). Channels with empty env vars are skipped. Errors per channel are logged but don't fail the Lambda.

### GetGameStatsFunction
Invoked by EventBridge Scheduler after a game ends. Calls the Riot Match-V5 API, extracts the tracked player's stats (champion, KDA, CS, damage, gold, vision, items, etc.), and updates the DynamoDB record.

## Infrastructure

- **DynamoDB** - Stores match IDs for deduplication and post-game stats
- **SNS** - Fan-out topic for game detection notifications
- **SES** - Sends game detection emails
- **EventBridge Scheduler** - Triggers delayed stats collection (auto-deletes after execution)

## Deploy

```
sam build && sam deploy --guided
```

Required parameters: `RiotApiKey`, `SesSenderEmail`, `PlayerName`, `TagLine`, `RecipientEmail`.

Optional parameters: `DiscordWebhookUrl` (leave empty to skip Discord notifications).
