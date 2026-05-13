package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"

	"github.com/mamidevs/flotilla/internal/config"
	"github.com/mamidevs/flotilla/internal/worker"
)

// adminBaseURL resolves where /ip, /nodes, /health live. Honors
// FLOTILLA_ADMIN_URL env var, else falls back to the default admin addr.
func adminBaseURL(override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	if v := strings.TrimSpace(os.Getenv("FLOTILLA_ADMIN_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://" + config.DefaultAdminAddr
}

func ipMain(args []string) {
	fs := pflag.NewFlagSet("ip", pflag.ContinueOnError)
	node := fs.String("node", "", "Only fetch this node's egress IP.")
	all := fs.Bool("all", false, "Print every node, even when only one is requested.")
	urlFlag := fs.String("admin-url", "", "Override admin URL (default $FLOTILLA_ADMIN_URL or http://127.0.0.1:8080).")
	_ = all
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	base := adminBaseURL(*urlFlag)
	url := base + "/ip"
	if *node != "" {
		url += "?node=" + *node
	}

	res, err := httpGetJSON(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	m, ok := res.(map[string]any)
	if !ok {
		fmt.Fprintln(os.Stderr, "unexpected response shape")
		os.Exit(1)
	}
	if len(m) == 0 {
		fmt.Println("(no workers reported)")
		return
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NODE\tEGRESS IP")
	for _, n := range names {
		_, _ = fmt.Fprintf(tw, "%s\t%v\n", n, m[n])
	}
	_ = tw.Flush()
}

func nodesMain(args []string) {
	fs := pflag.NewFlagSet("nodes", pflag.ContinueOnError)
	urlFlag := fs.String("admin-url", "", "Override admin URL.")
	jsonOut := fs.Bool("json", false, "Output raw JSON.")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	base := adminBaseURL(*urlFlag)
	res, err := httpGetJSON(base + "/health/nodes")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return
	}

	m, ok := res.(map[string]any)
	if !ok {
		fmt.Fprintln(os.Stderr, "unexpected shape")
		os.Exit(1)
	}
	raw, _ := json.Marshal(m["nodes"])
	var nodes []worker.Status
	_ = json.Unmarshal(raw, &nodes)

	tw := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tEXIT\tHEALTHY\tACTIVE\tEGRESS\tDNS")
	for _, n := range nodes {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%v\t%d\t%s\t%s\n", n.Name, n.ExitNode, n.Healthy, n.Active, n.EgressIP, n.DNSName)
	}
	_ = tw.Flush()
}

func doctorMain(args []string) {
	fs := pflag.NewFlagSet("doctor", pflag.ContinueOnError)
	configPath := fs.StringP("config", "c", "flotilla.yaml", "Path to flotilla.yaml.")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	if _, err := os.Stat(*configPath); err != nil {
		fmt.Println("[FAIL] config not found:", *configPath)
		os.Exit(1)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Println("[FAIL] config invalid:", err)
		os.Exit(1)
	}
	fmt.Println("[ OK ] config parses + validates")
	fmt.Printf("       %d node(s), strategy=%s\n", len(cfg.Nodes), cfg.Dispatch.Strategy)

	switch cfg.Auth.Method {
	case config.AuthMethodAuthKey:
		key := strings.TrimSpace(cfg.Auth.AuthKey)
		if key == "" && strings.TrimSpace(os.Getenv("TS_AUTHKEY")) == "" && strings.TrimSpace(os.Getenv("TS_AUTH_KEY")) == "" {
			fmt.Println("[WARN] auth.method=authkey but no key found (neither config nor TS_AUTHKEY env)")
		} else {
			fmt.Println("[ OK ] auth.method=authkey, key available")
		}
	case config.AuthMethodOAuth2:
		if tok, _ := authTokenFromEnv(); tok != "" {
			fmt.Println("[ OK ] TS_OAUTH_ACCESS_TOKEN env set")
		} else {
			path := cfg.Auth.OAuth2Credentials
			if path == "" {
				path = "~/.config/flotilla/oauth2.json"
			}
			fmt.Printf("[INFO] looking for OAuth2 credentials at %s\n", path)
		}
	}

	base := adminBaseURL("")
	if _, err := httpGetJSON(base + "/health"); err != nil {
		fmt.Println("[INFO] admin endpoint not reachable at", base, "— that's fine if the daemon isn't running")
	} else {
		fmt.Println("[ OK ] admin endpoint healthy at", base)
	}
}

func authTokenFromEnv() (token, tag string) {
	token = strings.TrimSpace(os.Getenv("TS_OAUTH_ACCESS_TOKEN"))
	tag = strings.TrimSpace(os.Getenv("TS_OAUTH_TAG"))
	return
}

func httpGetJSON(url string) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var v any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}
