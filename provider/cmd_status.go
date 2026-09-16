package provider

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
)

type Status struct {
}

func (self *Status) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	type WarpStatusResult struct {
		Version       string `json:"version,omitempty"`
		ConfigVersion string `json:"config_version,omitempty"`
		Status        string `json:"status"`
		ClientAddress string `json:"client_address,omitempty"`
		Host          string `json:"host"`
	}

	result := &WarpStatusResult{
		Version: RequireVersion(),
		// ConfigVersion: RequireConfigVersion(),
		Status: "ok",
		Host:   RequireHost(),
	}

	responseJson, err := json.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(responseJson)
}

func Host() (string, error) {
	host := os.Getenv("WARP_HOST")
	if host != "" {
		return host, nil
	}
	host, err := os.Hostname()
	if err == nil {
		return host, nil
	}
	return "", errors.New("WARP_HOST not set")
}

func RequireHost() string {
	host, err := Host()
	if err != nil {
		panic(err)
	}
	return host
}
