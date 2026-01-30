package pushover

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

const endpoint = "https://api.pushover.net/1/messages.json"

type Payload struct {
	Token   string `json:"token"`
	User    string `json:"user"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message"`
}

func Send(payload Payload) error {
	if payload.Token == "" || payload.User == "" {
		return fmt.Errorf("pushover token/user missing")
	}
	buf, _ := json.Marshal(payload)
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("pushover error: %s", resp.Status)
	}
	return nil
}
