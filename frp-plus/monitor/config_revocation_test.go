package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/fatedier/frp/extension/frpmonitor/shared"
	"github.com/gorilla/websocket"
)

// Revocation stops new delivery. A prepare already received by the authenticated
// peer remains unknown until its journal can be queried; node deletion must not
// erase that audit or pretend that the Agent cancelled its local operation.
func TestConfigRevocationRetainsDispatchedOperation(t *testing.T) {
	for _, action := range []string{"rotate", "delete"} {
		t.Run(action, func(t *testing.T) {
			s, token, admin := testAdmin(t)
			cookie, session := login(t, s, admin)
			peer := dialConfig(t, s, token, "revocation-session", false)
			input := configAdminInput()
			input.SecretValues = []shared.ConfigSecretInput{}
			input.Changes = []shared.ConfigChange{{Operation: "create", Kind: "proxy", Name: "pending-revocation", Type: "tcp", Fields: []shared.ConfigFieldPatch{}, Secrets: []shared.ConfigSecretPatch{}}}
			body, err := json.Marshal(input)
			if err != nil {
				t.Fatal("cannot encode bounded revocation fixture")
			}
			request, err := http.NewRequest(http.MethodPost, "http://"+s.Address()+configAdminPath+"/operations", bytes.NewReader(body))
			if err != nil {
				t.Fatal("cannot prepare revocation request")
			}
			request.AddCookie(cookie)
			request.Header.Set("Origin", "http://"+s.Address())
			request.Header.Set("X-CSRF-Token", session.CSRF)
			request.Header.Set("Content-Type", "application/json")
			type outcome struct {
				status int
				value  configOperationResponse
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
				if err != nil {
					done <- outcome{err: err}
					return
				}
				defer response.Body.Close()
				var value configOperationResponse
				err = json.NewDecoder(io.LimitReader(response.Body, 512*1024)).Decode(&value)
				done <- outcome{status: response.StatusCode, value: value, err: err}
			}()
			command := readConfig(t, peer)
			if command.Action != "prepare" {
				t.Fatal("expected a dispatched prepare")
			}
			before, err := s.control.GetConfigOperation(context.Background(), command.OperationID)
			if err != nil || before.State != "draft" {
				t.Fatal("command dispatched before durable intent")
			}
			method, path, status := http.MethodPost, "/api/admin/v1/nodes/1/rotate", 200
			if action == "delete" {
				method, path, status = http.MethodDelete, "/api/admin/v1/nodes/1", 204
			}
			response := adminRequest(t, s, method, path, "", cookie, session.CSRF, http.Header{"Origin": []string{"http://" + s.Address()}})
			response.Body.Close()
			if response.StatusCode != status {
				t.Fatal("credential revocation failed")
			}
			select {
			case result := <-done:
				if result.err != nil || result.status != 201 || result.value.Operation == nil || result.value.Operation.State != "outcome_unknown" || result.value.Agent != nil {
					t.Fatal("revoked dispatched operation fabricated a terminal result")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("revocation did not release the pending HTTP command")
			}
			after, err := s.control.GetConfigOperation(context.Background(), command.OperationID)
			if err != nil || after.State != "outcome_unknown" || after.ServiceID != input.ServiceID || after.Creator != "operator" {
				t.Fatal("revocation lost durable operation identity or audit")
			}
			active, err := s.control.ListActiveConfigOperations(context.Background(), "", 100)
			if err != nil || len(active) != 1 || active[0].OperationID != command.OperationID {
				t.Fatal("revocation silently released unknown operation")
			}
			if _, err = s.ConfigCommand(context.Background(), "1", shared.ConfigCommand{Action: "inspect"}); !errors.Is(err, ErrConfigNotSent) || !errors.Is(err, ErrConfigUnavailable) {
				t.Fatal("revoked node accepted a new command")
			}
			connection, denied, err := websocket.DefaultDialer.Dial("ws://"+s.Address()+"/agent/v1/ws", http.Header{"Authorization": []string{"Bearer " + token}})
			if connection != nil {
				connection.Close()
			}
			if denied != nil {
				denied.Body.Close()
			}
			if err == nil || denied == nil || denied.StatusCode != http.StatusUnauthorized {
				t.Fatal("old credential reconnected after revocation")
			}
		})
	}
}
