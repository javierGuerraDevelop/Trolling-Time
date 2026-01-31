import os
import json
import uuid
import boto3
import requests
from datetime import datetime

dynamodb = boto3.resource("dynamodb", region_name="us-east-1")
ses_client = boto3.client("ses", region_name="us-east-1")

players_table_name = os.environ["PLAYERS_TABLE"]
email_logs_table_name = os.environ["EMAIL_LOGS_TABLE"]
riot_api_key = os.environ["RIOT_API_KEY"]
alert_emails = os.environ.get("ALERT_EMAILS", "").split(",")
sender_email = os.environ.get("SENDER_EMAIL", "")


def lambda_handler(event, context):
    # Check if tracked players are in game
    try:
        players_table = dynamodb.Table(players_table_name)

        # Get all players from DynamoDB
        response = players_table.scan()
        players = response.get("Items", [])

        if not players:
            print("No players to track")
            return {"statusCode": 200, "body": json.dumps({"message": "No players to track"})}

        checked_players = []
        in_game_players = []

        for player in players:
            player_id = player["player_id"]
            puuid = player.get("puuid")
            region = player["region"]
            name = player["name"]

            # Skip if no PUUID
            if not puuid:
                print(f"No PUUID for {player_id}, skipping")
                continue

            checked_players.append(player_id)
            in_game, game_data = is_player_in_game(puuid, region)

            if in_game:
                print(f"{player_id} is in game!")
                in_game_players.append(player_id)

                # Send alerts to through email
                for email in alert_emails:
                    if email.strip():
                        send_alert(email.strip(), game_data, puuid, name)

            # Update timestamp
            players_table.update_item(
                Key={"player_id": player_id},
                UpdateExpression="SET last_checked = :timestamp",
                ExpressionAttributeValues={
                    ":timestamp": datetime.utcnow().isoformat()},
            )

        return {
            "statusCode": 200,
            "body": json.dumps(
                {
                    "message": "Check completed",
                    "checked_players": checked_players,
                    "in_game_players": in_game_players,
                    "timestamp": datetime.utcnow().isoformat(),
                }
            ),
        }

    except Exception as e:
        print(f"Error in check_players handler: {str(e)}")
        return {"statusCode": 500, "body": json.dumps({"message": "Error checking players", "error": str(e)})}


def is_player_in_game(puuid, summoner_region):
    # Check if player is in game
    url = f"https://{summoner_region}.api.riotgames.com/lol/spectator/v4/active-games/by-summoner/{puuid}"
    headers = {"X-Riot-Token": riot_api_key}

    try:
        response = requests.get(url, headers=headers)

        if response.status_code == 200:
            game_data = response.json()
            print(f"Player {puuid} is in game!")
            return True, game_data
        elif response.status_code == 404:
            print(f"Player {puuid} is not in game")
            return False, None
        else:
            print(f"Error checking game status: {response.status_code}")
            return False, None

    except requests.exceptions.RequestException as e:
        print(f"Error checking if player in game: {e}")
        return False, None


def send_alert(recipient_email, game_data, tracked_puuid, player_name):
    # Send email alert and log to dynamo
    try:
        game_mode = game_data.get("gameMode", "Unknown")
        game_type = game_data.get("gameType", "Unknown")

        # Get player name from participants
        for participant in game_data.get("participants", []):
            if participant.get("puuid") == tracked_puuid:
                player_name = participant.get("summonerName", player_name)
                break

        # Create email
        subject = f"{player_name} is in game!"
        body_text = f"""
Player: {player_name}
Game Mode: {game_mode}
Game Type: {game_type}
Time: {datetime.utcnow().strftime('%Y-%m-%d %H:%M:%S')} UTC

The player you're tracking is currently in a League of Legends game!
"""

        body_html = f"""
<html>
<head></head>
<body>
  <h2>{player_name} is in game!</h2>
  <p><strong>Player:</strong> {player_name}</p>
  <p><strong>Game Mode:</strong> {game_mode}</p>
  <p><strong>Game Type:</strong> {game_type}</p>
  <p><strong>Time:</strong> {datetime.utcnow().strftime('%Y-%m-%d %H:%M:%S')} UTC</p>
  <p>The player you're tracking is currently in a League of Legends game!</p>
</body>
</html>
"""

        # Send email using SES
        response = ses_client.send_email(
            Source=sender_email,
            Destination={"ToAddresses": [recipient_email]},
            Message={
                "Subject": {"Data": subject, "Charset": "UTF-8"},
                "Body": {"Text": {"Data": body_text, "Charset": "UTF-8"}, "Html": {"Data": body_html, "Charset": "UTF-8"}},
            },
        )

        print(f"Email sent to {recipient_email}, MessageId: {
              response['MessageId']}")

        # Log to dynamo
        email_logs_table = dynamodb.Table(email_logs_table_name)
        log_id = str(uuid.uuid4())
        timestamp = datetime.utcnow().isoformat()

        email_logs_table.put_item(
            Item={
                "log_id": log_id,
                "timestamp": timestamp,
                "recipient_email": recipient_email,
                "player_name": player_name,
                "game_mode": game_mode,
                "game_type": game_type,
                "message_id": response["MessageId"],
            }
        )

        print(f"Email log saved to DynamoDB: {log_id}")

    except Exception as e:
        print(f"Error sending email to {recipient_email}: {e}")
