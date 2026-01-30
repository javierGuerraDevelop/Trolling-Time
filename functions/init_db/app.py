import os
import json
import boto3
from datetime import datetime

dynamodb = boto3.resource("dynamodb", region_name="us-east-1")
players_table_name = os.environ["PLAYERS_TABLE"]
email_logs_table_name = os.environ["EMAIL_LOGS_TABLE"]


def lambda_handler(event, context):
    # Initialize DynamoDB with players
    try:
        players_table = dynamodb.Table(players_table_name)

        players_data = event.get("players", [])

        # Try to get from dotenv
        if not players_data:
            player_names = os.getenv("PLAYER_NAMES", "").split(",")
            player_regions = os.getenv("PLAYER_REGIONS", "").split(",")

            if len(player_names) == len(player_regions):
                players_data = [
                    {"name": name.strip(), "region": region.strip()}
                    for name, region in zip(player_names, player_regions)
                    if name.strip() and region.strip()
                ]

        initialized_players = []

        for player in players_data:
            player_name = player.get("name")
            region = player.get("region")

            if not player_name or not region:
                continue

            # Create player_id
            player_id = f"{player_name}#{region}"

            # Check if player exists
            response = players_table.get_item(Key={"player_id": player_id})

            if "Item" not in response:
                item = {
                    "player_id": player_id,
                    "name": player_name,
                    "region": region,
                    "puuid": None,
                    "last_checked": None,
                    "created_at": datetime.utcnow().isoformat(),
                    "updated_at": datetime.utcnow().isoformat(),
                }

                players_table.put_item(Item=item)
                initialized_players.append(player_id)
                print(f"Initialized player: {player_id}")
            else:
                print(f"Player already exists: {player_id}")

        return {
            "statusCode": 200,
            "body": json.dumps(
                {
                    "message": "Database initialized successfully",
                    "initialized_players": initialized_players,
                    "total_players": len(players_data),
                }
            ),
        }

    except Exception as e:
        print(f"Error initializing database: {str(e)}")
        return {"statusCode": 500, "body": json.dumps({"message": "Error initializing database", "error": str(e)})}
