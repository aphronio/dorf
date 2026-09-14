package codex

import "encoding/json"

// Codex toolOutput does not retain clientUserMessageId. Keep the delivery
// identity inside its persisted output for input attribution across cold reads.
type applicationObservation struct {
	DeliveryID string `json:"delivery_id"`
	Content    string `json:"content"`
}

func nativeObservation(deliveryID, text string) map[string]any {
	output, _ := json.Marshal(applicationObservation{DeliveryID: deliveryID, Content: text})
	return map[string]any{"name": "application_observation", "namespace": "dorf", "output": string(output)}
}

func observationDeliveryID(item map[string]any) string {
	if item["type"] != "functionCallOutput" || item["name"] != "application_observation" || item["namespace"] != "dorf" {
		return ""
	}
	var output applicationObservation
	if json.Unmarshal([]byte(stringValue(item["output"])), &output) != nil || output.Content == "" {
		return ""
	}
	return output.DeliveryID
}
