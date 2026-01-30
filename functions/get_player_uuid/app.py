import os
import json
import boto3
import requests
from datetime import datetime

dynamodb = boto3.resource("dynamodb", region_name="us-east-1")
players_table_name = os.environ["PLAYERS_TABLE"]
riot_api_key = os.environ["RIOT_API_KEY"]


def lambda_handler(event, context):
    # Get PUUID and put in update DynamoDB.
    try:
        players_table = dynamodb.Table(players_table_name)

        # Check if payload provided to get a specific player
        player_name = event.get("player_name")
        region = event.get("region")

        if player_name and region:  # Process single player
            puuid = get_player_uuid(player_name, region)
            if puuid:
                player_id = f"{player_name}#{region}"
                update_player_puuid(players_table, player_id,
                                    puuid, player_name, region)
                return {"statusCode": 200, "body": json.dumps({"player_id": player_id, "puuid": puuid})}
            else:
                return {"statusCode": 404, "body": json.dumps({"message": f"Could not find PUUID for {player_name}"})}
        else:  # Process all players in table
            response = players_table.scan()
            updated_players = []

            for item in response.get("Items", []):
                if not item.get("puuid"):
                    name = item["name"]
                    region = item["region"]
                    puuid = get_player_uuid(name, region)

                    if puuid:
                        update_player_puuid(
                            players_table, item["player_id"], puuid, name, region)
                        updated_players.append(item["player_id"])

            return {"statusCode": 200, "body": json.dumps({"message": "PUUIDs updated", "updated_players": updated_players})}

    except Exception as e:
        print(f"Error in get_player_uuid handler: {str(e)}")
        return {"statusCode": 500, "body": json.dumps({"message": "Error getting player UUID", "error": str(e)})}


def get_player_uuid(summoner_name, summoner_region):
    # Get player PUUID from summoner name + region
    url = f"https://{summoner_region}.api.riotgames.com/lol/summoner/v4/summoners/by-name/{summoner_name}"
    headers = {"X-Riot-Token": riot_api_key}

    try:
        response = requests.get(url, headers=headers)
        response.raise_for_status()
        data = response.json()
        puuid = data.get("puuid")
        print(f"Got PUUID for {summoner_name}: {puuid}")
        return puuid
    except requests.exceptions.RequestException as e:
        print(f"Error getting PUUID for {summoner_name}: {e}")
        return None


def update_player_puuid(table, player_id, puuid, name, region):
    # Update player PUUID in DynamoDB
    table.update_item(
        Key={"player_id": player_id},
        UpdateExpression="SET puuid = :puuid, updated_at = :updated_at",
        ExpressionAttributeValues={":puuid": puuid,
                                   ":updated_at": datetime.utcnow().isoformat()},
    )
    print(f"Updated PUUID for {player_id}")
