package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/instance"
)

type Client struct {
	Election *instance.Election
	HTTP     *http.Client
}

func NewClient(election *instance.Election) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	// Message streams and run-once orchestration deliberately wait on a
	// provider. Their caller contexts own cancellation; a fixed response-header
	// timeout would turn ordinary long harness work into a false transport
	// failure before the server can return its first event or final status.
	transport.ResponseHeaderTimeout = 0
	return &Client{
		Election: election,
		HTTP:     &http.Client{Transport: transport},
	}
}

func (c *Client) WaitReady(ctx context.Context) (app.SharedState, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := c.State(ctx)
		if err == nil {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return app.SharedState{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) Handle(ctx context.Context, message core.Message, emit core.Emit) error {
	message.InstanceID = c.Election.ID()
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	response, err := c.request(ctx, http.MethodPost, "/v1/message", body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := responseError(response); err != nil {
		return err
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var envelope streamEnvelope
		if err := decoder.Decode(&envelope); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("read workspace server response: %w", err)
		}
		if envelope.Error != "" {
			return errors.New(envelope.Error)
		}
		if envelope.Event != nil && emit != nil {
			emit(*envelope.Event)
		}
	}
}

func (c *Client) State(ctx context.Context) (app.SharedState, error) {
	response, err := c.request(ctx, http.MethodGet, "/v1/state", nil)
	if err != nil {
		return app.SharedState{}, err
	}
	defer response.Body.Close()
	if err := responseError(response); err != nil {
		return app.SharedState{}, err
	}
	var state app.SharedState
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		return app.SharedState{}, err
	}
	return state, nil
}

func (c *Client) Notify(ctx context.Context, origin, message string) (string, error) {
	body, err := json.Marshal(notifyRequest{Origin: origin, Message: message})
	if err != nil {
		return "", err
	}
	response, err := c.request(ctx, http.MethodPost, "/v1/notify", body)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if err := responseError(response); err != nil {
		return "", err
	}
	var result notifyResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.ID, nil
}

func (c *Client) AckNotification(ctx context.Context, origin, id string, afterChars int) error {
	body, err := json.Marshal(notificationAckRequest{Origin: origin, ID: id, AfterChars: afterChars})
	if err != nil {
		return err
	}
	response, err := c.request(ctx, http.MethodPost, "/v1/notification-ack", body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return responseError(response)
}

func (c *Client) Status(ctx context.Context, conversation string) (app.StatusSnapshot, error) {
	query := url.Values{}
	query.Set("conversation", conversation)
	query.Set("instance_id", c.Election.ID())
	response, err := c.request(ctx, http.MethodGet, "/v1/status?"+query.Encode(), nil)
	if err != nil {
		return app.StatusSnapshot{}, err
	}
	defer response.Body.Close()
	if err := responseError(response); err != nil {
		return app.StatusSnapshot{}, err
	}
	var status app.StatusSnapshot
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return app.StatusSnapshot{}, err
	}
	return status, nil
}

func (c *Client) InitialScreen(ctx context.Context, hasHistory, forceWelcome bool) (*core.Screen, error) {
	path := "/v1/initial-screen?has_history=" + strconv.FormatBool(hasHistory) + "&welcome=" + strconv.FormatBool(forceWelcome)
	response, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := responseError(response); err != nil {
		return nil, err
	}
	var result screenResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Screen, nil
}

func (c *Client) ScreenAction(ctx context.Context, screenID, action string, values map[string]string) (*core.Screen, error) {
	body, err := json.Marshal(screenActionRequest{ScreenID: screenID, Action: action, Values: values})
	if err != nil {
		return nil, err
	}
	response, err := c.request(ctx, http.MethodPost, "/v1/screen-action", body)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := responseError(response); err != nil {
		return nil, err
	}
	var result screenResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Screen, nil
}

func (c *Client) ApplySettings(ctx context.Context, values map[string]string) error {
	body, err := json.Marshal(settingsRequest{Values: values})
	if err != nil {
		return err
	}
	response, err := c.request(ctx, http.MethodPost, "/v1/settings", body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return responseError(response)
}

// Diagnostic forwards a bounded local-surface observation to the elected
// owner's structured runtime logger. Callers keep this best-effort and off
// their event loop.
func (c *Client) Diagnostic(ctx context.Context, event, message string) error {
	body, err := json.Marshal(diagnosticRequest{Event: event, Message: message})
	if err != nil {
		return err
	}
	response, err := c.request(ctx, http.MethodPost, "/v1/diagnostic", body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return responseError(response)
}

func (c *Client) RunOnce(ctx context.Context) error {
	response, err := c.request(ctx, http.MethodPost, "/v1/run-once", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return responseError(response)
}

func (c *Client) request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	lease, err := c.Election.Current()
	if err != nil {
		return nil, fmt.Errorf("locate workspace server: %w", err)
	}
	base, err := loopbackURL(lease.Endpoint)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+lease.Token)
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	return c.HTTP.Do(request)
}

func loopbackURL(endpoint string) (string, error) {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid workspace server endpoint: %w", err)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("workspace server endpoint is not loopback")
	}
	return (&url.URL{Scheme: "http", Host: endpoint}).String(), nil
}

func responseError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	detail := strings.TrimSpace(string(data))
	if detail == "" {
		detail = response.Status
	}
	return fmt.Errorf("workspace server: %s", detail)
}
