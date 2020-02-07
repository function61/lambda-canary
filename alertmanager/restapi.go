package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/function61/gokit/jsonfile"
	"github.com/function61/lambda-alertmanager/alertmanager/pkg/alertmanagertypes"
	"github.com/function61/lambda-alertmanager/alertmanager/pkg/apigatewayutils"
	"net/http"
	"os"
	"sort"
	"time"
)

func handleRestCall(ctx context.Context, req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	synopsis := req.HTTPMethod + " " + req.Path

	switch synopsis {
	case "GET /alerts":
		return handleGetAlerts(ctx, req)
	case "GET /alerts/acknowledge":
		// this endpoint should really be a POST (since it mutates state), but we've to be
		// pragmatic here because we want acks to be ack-able from emails
		key := req.QueryStringParameters["key"]
		if key == "" {
			return apigatewayutils.BadRequest("key not specified"), nil
		}

		return handleAcknowledgeAlert(ctx, key)
	case "POST /alerts/ingest":
		item := alertmanagertypes.Alert{}
		if err := jsonfile.Unmarshal(bytes.NewBufferString(req.Body), &item, true); err != nil {
			return apigatewayutils.BadRequest(err.Error()), nil
		}

		created, err := ingestAlert(item)
		if err != nil {
			return apigatewayutils.InternalServerError(err.Error()), nil
		}

		if created {
			return apigatewayutils.Created(), nil
		} else {
			return apigatewayutils.NoContent(), nil
		}
	case "GET /deadmansswitch/checkin": // /deadmansswitch/checkin?subject=ubackup_done&ttl=24h30m
		// same semantic hack here as acknowledge endpoint
		return handleDeadMansSwitchCheckin(ctx, alertmanagertypes.DeadMansSwitchCheckinRequest{
			Subject: req.QueryStringParameters["subject"],
			TTL:     req.QueryStringParameters["ttl"],
		})
	case "POST /deadmansswitch/checkin": // {"subject":"ubackup_done","ttl":"24h30m"}
		checkin := alertmanagertypes.DeadMansSwitchCheckinRequest{}
		if err := jsonfile.Unmarshal(bytes.NewBufferString(req.Body), &checkin, true); err != nil {
			return apigatewayutils.BadRequest(err.Error()), nil
		}

		return handleDeadMansSwitchCheckin(ctx, checkin)
	case "GET /deadmansswitches":
		return handleGetDeadMansSwitches(ctx, req)
	case "POST /prometheus-alertmanager/api/v1/alerts":
		return apigatewayutils.InternalServerError("not implemented yet"), nil
	default:
		return apigatewayutils.BadRequest("unknown endpoint"), nil
	}
}

func handleGetAlerts(ctx context.Context, req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	alerts, err := getFiringAlerts()
	if err != nil {
		return apigatewayutils.InternalServerError(err.Error()), nil
	}

	return apigatewayutils.RespondJson(alerts)
}

func handleAcknowledgeAlert(ctx context.Context, alertKey string) (*events.APIGatewayProxyResponse, error) {
	if _, err := dynamodbSvc.DeleteItem(&dynamodb.DeleteItemInput{
		TableName:           alertsDynamoDbTableName,
		ConditionExpression: aws.String("attribute_exists(alert_key)"), // to get error if item-to-delete not found
		Key: dynamoDbRecord{
			"alert_key": mkDynamoString(alertKey),
		},
	}); err != nil {
		if errAws, ok := err.(awserr.Error); ok && errAws.Code() == dynamodb.ErrCodeConditionalCheckFailedException {
			// API Gateway doesn't process APIGatewayProxyResponse if Lambda is reported as failure
			return apigatewayutils.NotFound(fmt.Sprintf("alert %s does not exist", alertKey)), nil
		} else {
			return apigatewayutils.InternalServerError(err.Error()), nil
		}
	}

	return apigatewayutils.OkText(fmt.Sprintf("Ack ok for %s", alertKey))
}

func getFiringAlerts() ([]alertmanagertypes.Alert, error) {
	result, err := dynamodbSvc.Scan(&dynamodb.ScanInput{
		TableName: alertsDynamoDbTableName,
		Limit:     aws.Int64(1000), // whichever comes first, 1 MB or 1 000 records
	})
	if err != nil {
		return nil, err
	}

	alerts := []alertmanagertypes.Alert{}

	for _, alertDb := range result.Items {
		alert := alertmanagertypes.Alert{}
		if err := unmarshalFromDynamoDb(alertDb, &alert); err != nil {
			return nil, err
		}

		alerts = append(alerts, alert)
	}

	sort.Slice(alerts, func(i, j int) bool {
		return alerts[i].Key < alerts[j].Key
	})

	return alerts, nil
}

func handleGetDeadMansSwitches(ctx context.Context, req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	switches, err := getDeadMansSwitches()
	if err != nil {
		return apigatewayutils.InternalServerError(err.Error()), nil
	}

	return apigatewayutils.RespondJson(switches)
}

func handleDeadMansSwitchCheckin(ctx context.Context, raw alertmanagertypes.DeadMansSwitchCheckinRequest) (*events.APIGatewayProxyResponse, error) {
	if raw.Subject == "" || raw.TTL == "" {
		return apigatewayutils.BadRequest("subject or ttl empty"), nil
	}

	now := time.Now()

	ttl, err := parseTtlSpec(raw.TTL, now)
	if err != nil {
		return apigatewayutils.BadRequest(err.Error()), nil
	}

	deadMansSwitch := alertmanagertypes.DeadMansSwitch{
		Subject: raw.Subject,
		TTL:     ttl,
	}

	dynamoItem, err := marshalToDynamoDb(&deadMansSwitch)
	if err != nil {
		return apigatewayutils.InternalServerError(err.Error()), nil
	}

	if _, err := dynamodbSvc.PutItem(&dynamodb.PutItemInput{
		TableName: dmsDynamoDbTableName,
		Item:      dynamoItem,
	}); err != nil {
		return apigatewayutils.InternalServerError(err.Error()), nil
	}

	alerts, err := getFiringAlerts()
	if err != nil {
		return apigatewayutils.InternalServerError(err.Error()), nil
	}

	comparableAlert := deadMansSwitch.AsAlert(now)
	var alertFiringFromDeadMansSwitch *alertmanagertypes.Alert
	for _, alert := range alerts {
		// what this switch would be as an alert? to see if this switch is currently firing,
		// so we can auto-ack it
		if alert.Equal(comparableAlert) {
			alertFiringFromDeadMansSwitch = &alert
			break
		}
	}

	if alertFiringFromDeadMansSwitch == nil {
		return apigatewayutils.OkText("Check-in noted")
	} else {
		if ackRes, err := handleAcknowledgeAlert(ctx, alertFiringFromDeadMansSwitch.Key); err != nil || (ackRes != nil && ackRes.StatusCode != http.StatusOK) {
			return ackRes, err
		}

		return apigatewayutils.OkText("Check-in noted; alert that was firing for this dead mans's switch was acked")
	}
}

func getDeadMansSwitches() ([]alertmanagertypes.DeadMansSwitch, error) {
	result, err := dynamodbSvc.Scan(&dynamodb.ScanInput{
		TableName: dmsDynamoDbTableName,
		Limit:     aws.Int64(1000), // whichever comes first, 1 MB or 1 000 records
	})
	if err != nil {
		return nil, err
	}

	switches := []alertmanagertypes.DeadMansSwitch{}

	for _, items := range result.Items {
		dms := alertmanagertypes.DeadMansSwitch{}
		if err := unmarshalFromDynamoDb(items, &dms); err != nil {
			return nil, err
		}

		switches = append(switches, dms)
	}

	sort.Slice(switches, func(i, j int) bool {
		return switches[i].Subject < switches[j].Subject
	})

	return switches, nil
}

func ackLink(alert alertmanagertypes.Alert) string {
	return os.Getenv("API_ENDPOINT") + "/alerts/acknowledge?key=" + alert.Key
}
