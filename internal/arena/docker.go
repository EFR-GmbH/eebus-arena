package arena

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Docker struct {
	client     *http.Client
	socket     string
	version    string
	serverInfo map[string]any
}

var ErrDockerNotFound = errors.New("Docker object not found")

func NewDocker() (*Docker, error) {
	socket := strings.TrimPrefix(os.Getenv("DOCKER_HOST"), "unix://")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	if !strings.HasPrefix(socket, "/") {
		return nil, fmt.Errorf("only unix:// Docker sockets are supported")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	d := &Docker{client: &http.Client{Transport: transport}, socket: socket}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var version map[string]any
	if err := d.JSON(ctx, "GET", "/version", nil, &version); err != nil {
		return nil, err
	}
	d.version = "/v" + fmt.Sprint(version["ApiVersion"])
	d.serverInfo = version
	return d, nil
}
func (d *Docker) Request(ctx context.Context, method, path string, input any) (*http.Response, error) {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+d.version+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	// Docker lifecycle POSTs use 304 when the requested state already holds.
	if response.StatusCode >= 300 && !(method == http.MethodPost && response.StatusCode == http.StatusNotModified) {
		defer response.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		if response.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("Docker %s %s: %w: %s", method, path, ErrDockerNotFound, b)
		}
		return nil, fmt.Errorf("Docker %s %s: %s", method, path, b)
	}
	return response, nil
}
func (d *Docker) JSON(ctx context.Context, method, path string, input, output any) error {
	response, err := d.Request(ctx, method, path, input)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if output == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	return json.NewDecoder(response.Body).Decode(output)
}
func (d *Docker) Containers(ctx context.Context, runID string) ([]map[string]any, error) {
	return d.containersWithLabel(ctx, "org.eebus-arena.run="+runID)
}
func (d *Docker) ProjectContainers(ctx context.Context, runID string) ([]map[string]any, error) {
	return d.containersWithLabel(ctx, "com.docker.compose.project=eebus-"+runID)
}
func (d *Docker) containersWithLabel(ctx context.Context, label string) ([]map[string]any, error) {
	filter, _ := json.Marshal(map[string][]string{"label": {label}})
	var containers []map[string]any
	err := d.JSON(ctx, "GET", "/containers/json?all=1&filters="+url.QueryEscape(string(filter)), nil, &containers)
	return containers, err
}
func (d *Docker) Owned(ctx context.Context, id, runID, nodeID string) error {
	var c struct {
		Config struct{ Labels map[string]string }
	}
	if err := d.JSON(ctx, "GET", "/containers/"+id+"/json", nil, &c); err != nil {
		return err
	}
	if c.Config.Labels["org.eebus-arena.run"] != runID || c.Config.Labels["org.eebus-arena.node"] != nodeID {
		return fmt.Errorf("container ownership does not match selected node")
	}
	return nil
}
func (d *Docker) Action(ctx context.Context, id, action string) error {
	paths := map[string]string{"start": "start", "stop": "stop?t=10", "kill": "kill?signal=SIGKILL", "restart": "restart?t=10", "pause": "pause", "resume": "unpause"}
	path, ok := paths[action]
	if !ok {
		return fmt.Errorf("unknown lifecycle action")
	}
	return d.JSON(ctx, "POST", "/containers/"+id+"/"+path, nil, nil)
}
func (d *Docker) Send(ctx context.Context, id string, command Command) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", d.socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	req, _ := http.NewRequest("POST", "http://docker"+d.version+"/containers/"+id+"/attach?stream=1&stdin=1&stdout=0&stderr=0", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if err = req.Write(conn); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return err
	}
	if resp.StatusCode != 101 && resp.StatusCode != 200 {
		return fmt.Errorf("attach returned %s", resp.Status)
	}
	return json.NewEncoder(conn).Encode(command)
}
func (d *Docker) Logs(ctx context.Context, id string, since time.Time, consume func(byte, string)) error {
	path := "/containers/" + id + "/logs?follow=1&stdout=1&stderr=1&timestamps=1"
	if !since.IsZero() {
		path += "&since=" + strconv.FormatInt(since.Unix(), 10)
	}
	response, err := d.Request(ctx, "GET", path, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	buffers := map[byte]string{}
	discarding := map[byte]bool{}
	header := make([]byte, 8)
	for {
		if _, err = io.ReadFull(response.Body, header); err != nil {
			return err
		}
		size := binary.BigEndian.Uint32(header[4:])
		if size > 4<<20 {
			return fmt.Errorf("Docker log frame exceeds 4 MiB")
		}
		payload := make([]byte, size)
		if _, err = io.ReadFull(response.Body, payload); err != nil {
			return err
		}
		stream := header[0]
		// Docker prepends a timestamp to every json-file partial entry, including
		// continuations of a single adapter line larger than 16 KiB.
		if buffers[stream] != "" || discarding[stream] {
			if i := bytes.IndexByte(payload, ' '); i >= 0 {
				if _, parseErr := time.Parse(time.RFC3339Nano, string(payload[:i])); parseErr == nil {
					payload = payload[i+1:]
				}
			}
		}
		if discarding[stream] {
			end := bytes.IndexByte(payload, '\n')
			if end < 0 {
				continue
			}
			payload, discarding[stream] = payload[end+1:], false
		}
		buffers[stream] += string(payload)
		truncated := func(line string) {
			stamp, _, _ := strings.Cut(line, " ")
			// Keep the original Docker timestamp so backlog after this record
			// remains eligible for ingestion.
			consume(2, stamp+" log line exceeded 1 MiB and was truncated")
		}
		for {
			i := strings.IndexByte(buffers[stream], '\n')
			if i < 0 {
				break
			}
			line := buffers[stream][:i]
			if len(line) > 1<<20 {
				truncated(line)
			} else {
				consume(stream, line)
			}
			buffers[stream] = buffers[stream][i+1:]
		}
		if len(buffers[stream]) > 1<<20 {
			truncated(buffers[stream])
			buffers[stream], discarding[stream] = "", true
		}
	}
}
