package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func runCluster(args []string) {
	if len(args) == 0 {
		printClusterHelp()
		osExit(1)
		return
	}

	sub := args[0]
	subArgs := args[1:]

	switch sub {
	case "info":
		runClusterInfo(subArgs)
	case "status":
		runClusterStatus(subArgs)
	case "failover":
		runClusterFailover(subArgs)
	case "add-node":
		runClusterAddNode(subArgs)
	case "remove-node":
		runClusterRemoveNode(subArgs)
	case "enable":
		osExit(runClusterEnable(subArgs))
	case "disable":
		osExit(runClusterDisable(subArgs))
	default:
		fmt.Fprintf(os.Stderr, "Unknown cluster subcommand: %q\n", sub)
		printClusterHelp()
		osExit(1)
	}
}

func printClusterHelp() {
	fmt.Println("Usage: muninn cluster <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  info          Show cluster state (members, roles, epoch, cortex)")
	fmt.Println("  status        Show health + replication lag per node")
	fmt.Println("  failover      Trigger manual failover/election")
	fmt.Println("  add-node      Show instructions for adding a node to the cluster")
	fmt.Println("  remove-node   Show instructions for removing a node from the cluster")
	fmt.Println("  enable        Enable cluster mode on the running node")
	fmt.Println("  disable       Disable cluster mode on the running node")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --addr <url>   Server address (default: http://127.0.0.1:8475)")
	fmt.Println("  --json         Output raw JSON")
}

// runClusterInfo displays cluster topology and node status.
func runClusterInfo(args []string) {
	fs := flag.NewFlagSet("cluster info", flag.ContinueOnError)
	addr := fs.String("addr", "http://127.0.0.1:8475", "Server address")
	outputJSON := fs.Bool("json", false, "Output raw JSON")
	fs.Parse(args)

	resp, err := httpGet(*addr + "/v1/cluster/info")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to reach server at %s: %v\n", *addr, err)
		os.Exit(1)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(resp, &data); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid JSON response: %v\n", err)
		os.Exit(1)
	}

	// Check if cluster is disabled
	if enabled, ok := data["enabled"].(bool); ok && !enabled {
		fmt.Println("  Cluster mode is not enabled.")
		os.Exit(0)
	}

	if *outputJSON {
		fmt.Println(string(resp))
		return
	}

	// Table output
	nodeID := getString(data, "node_id")
	role := getString(data, "role")
	isLeader := getBool(data, "is_leader")
	epoch := getUint64(data, "epoch")
	cortexID := getString(data, "cortex_id")
	members := getMembers(data, "members")

	leaderMark := ""
	if isLeader {
		leaderMark = " (leader)"
	}

	fmt.Printf("\n  Node ID:    %s\n", nodeID)
	fmt.Printf("  Role:       %s%s\n", role, leaderMark)
	fmt.Printf("  Epoch:      %d\n", epoch)
	fmt.Printf("  Cortex ID:  %s\n", cortexID)
	fmt.Println()
	fmt.Println("  Members:")
	fmt.Println("    NODE                      ROLE         LAST_SEQ        ADDR")
	for _, m := range members {
		mid := getString(m, "node_id")
		mrole := getString(m, "role")
		lastSeq := getUint64(m, "last_seq")
		addr := getString(m, "addr")
		fmt.Printf("    %-26s %-12s %10d      %s\n", mid, mrole, lastSeq, addr)
	}
	fmt.Println()
}

// runClusterStatus shows cluster health and per-node replication lag.
func runClusterStatus(args []string) {
	fs := flag.NewFlagSet("cluster status", flag.ContinueOnError)
	addr := fs.String("addr", "http://127.0.0.1:8475", "Server address")
	outputJSON := fs.Bool("json", false, "Output raw JSON")
	fs.Parse(args)

	healthResp, err := httpGet(*addr + "/v1/cluster/health")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to reach server at %s: %v\n", *addr, err)
		os.Exit(1)
	}

	var health map[string]interface{}
	if err := json.Unmarshal(healthResp, &health); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid JSON response: %v\n", err)
		os.Exit(1)
	}

	nodesResp, err := httpGet(*addr + "/v1/cluster/nodes")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to reach server at %s: %v\n", *addr, err)
		os.Exit(1)
	}

	var nodesData map[string]interface{}
	if err := json.Unmarshal(nodesResp, &nodesData); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid JSON response: %v\n", err)
		os.Exit(1)
	}

	if *outputJSON {
		// Combined output
		combined := map[string]interface{}{
			"health": health,
			"nodes":  nodesData,
		}
		data, _ := json.MarshalIndent(combined, "", "  ")
		fmt.Println(string(data))
		return
	}

	// Table output
	status := getString(health, "status")
	role := getString(health, "role")
	isLeader := getBool(health, "is_leader")
	epoch := getUint64(health, "epoch")
	lag := getUint64(health, "replication_lag")

	leaderMark := ""
	if isLeader {
		leaderMark = " (leader)"
	}

	fmt.Printf("\n  Cluster Status: %s\n", status)
	fmt.Printf("  Role:           %s%s\n", role, leaderMark)
	fmt.Printf("  Epoch:          %d\n", epoch)
	fmt.Printf("  Replication Lag: %d\n", lag)
	fmt.Println()

	nodes := getMembers(nodesData, "nodes")
	if len(nodes) > 0 {
		fmt.Println("  Nodes:")
		fmt.Println("    NODE                      ROLE         LAG             LAST_SEQ")
		for _, n := range nodes {
			nid := getString(n, "node_id")
			nrole := getString(n, "role")
			nseq := getUint64(n, "last_seq")
			// For this table, we'd need per-node lag which requires calling /v1/replication/lag for each
			// For now, just show the info we have
			fmt.Printf("    %-26s %-12s %-15s %d\n", nid, nrole, "-", nseq)
		}
	}
	fmt.Println()
}

// runClusterFailover triggers a manual failover/election.
func runClusterFailover(args []string) {
	fs := flag.NewFlagSet("cluster failover", flag.ContinueOnError)
	addr := fs.String("addr", "http://127.0.0.1:8475", "Server address")
	yes := fs.Bool("yes", false, "Skip confirmation prompt")
	fs.Parse(args)

	if !*yes {
		fmt.Print("  ⚠  This will trigger a new election. Continue? (y/N): ")
		var confirm string
		fmt.Scanln(&confirm)
		if strings.ToLower(confirm) != "y" {
			fmt.Println("  Cancelled.")
			return
		}
	}

	resp, err := httpPost(*addr + "/v1/replication/promote")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: failed to reach server at %s: %v\n", *addr, err)
		osExit(1)
		return
	}

	var result map[string]interface{}
	if err := json.Unmarshal(resp, &result); err != nil {
		fmt.Fprintf(os.Stderr, "Error: invalid response: %v\n", err)
		osExit(1)
		return
	}

	if triggered := getBool(result, "triggered"); triggered {
		fmt.Println("  ✓ Election triggered.")
	} else {
		fmt.Println("  Election not triggered.")
		osExit(1)
	}
}

// runClusterAddNode shows instructions for adding a new node.
func runClusterAddNode(args []string) {
	fs := flag.NewFlagSet("cluster add-node", flag.ContinueOnError)
	fs.Parse(args)

	fmt.Println()
	fmt.Println("  ──────────────────────────────────────────────────────────────")
	fmt.Println("  Adding a New Node to the Cluster")
	fmt.Println("  ──────────────────────────────────────────────────────────────")
	fmt.Println()
	fmt.Println("  1. On the new machine, create a cluster.yaml file pointing to")
	fmt.Println("     one or more existing nodes as seed nodes:")
	fmt.Println()
	fmt.Println("     cluster.yaml:")
	fmt.Println("     ├── enabled: true")
	fmt.Println("     ├── node_id: \"my-new-node\"")
	fmt.Println("     ├── bind_addr: \":8474\"")
	fmt.Println("     └── seed_nodes:")
	fmt.Println("         └── - \"10.0.1.5:8474\"  # existing node")
	fmt.Println()
	fmt.Println("  2. Start muninn on the new machine:")
	fmt.Println()
	fmt.Println("     muninn start")
	fmt.Println()
	fmt.Println("  3. The node will auto-join the cluster, discover its role")
	fmt.Println("     (Cortex/Lobe/Sentinel/Observer), and begin replicating.")
	fmt.Println()
	fmt.Println("  ──────────────────────────────────────────────────────────────")
	fmt.Println()
}

// runClusterRemoveNode shows instructions for removing a node (not yet implemented).
func runClusterRemoveNode(args []string) {
	fs := flag.NewFlagSet("cluster remove-node", flag.ContinueOnError)
	nodeID := fs.String("node", "", "Node ID to remove")
	fs.Parse(args)

	if *nodeID == "" {
		fmt.Println("  Error: --node <nodeID> is required")
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("  ──────────────────────────────────────────────────────────────")
	fmt.Println("  Removing a Node from the Cluster")
	fmt.Println("  ──────────────────────────────────────────────────────────────")
	fmt.Println()
	fmt.Println("  ⚠  Node removal is not yet supported in the REST API.")
	fmt.Println()
	fmt.Println("  Workaround:")
	fmt.Println()
	fmt.Println("  1. Stop the target node:")
	fmt.Println("     ssh <node> muninn stop")
	fmt.Println()
	fmt.Println("  2. On the Cortex (leader), the node will be marked as down")
	fmt.Println("     after the heartbeat timeout (~30 seconds).")
	fmt.Println()
	fmt.Println("  3. To force removal, manually delete the node entry from the")
	fmt.Println("     coordinator's member list (internal implementation pending).")
	fmt.Println()
	fmt.Println("  ──────────────────────────────────────────────────────────────")
	fmt.Println()
}

// runClusterEnable enables cluster mode on a running node via the admin API.
func runClusterEnable(args []string) int {
	fs := flag.NewFlagSet("cluster enable", flag.ContinueOnError)
	addr := fs.String("addr", "http://127.0.0.1:8475", "MuninnDB admin address")
	role := fs.String("role", "primary", "Node role: primary|replica|sentinel|observer")
	bindAddr := fs.String("bind-addr", "", "Cluster bind address (IP:port)")
	cortexAddr := fs.String("cortex-addr", "", "Cortex address (required for replica/sentinel/observer)")
	secret := fs.String("secret", "", "Cluster secret")
	yes := fs.Bool("yes", false, "Skip confirmation prompt")

	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *bindAddr == "" {
		fmt.Fprintln(os.Stderr, "error: --bind-addr is required")
		fs.Usage()
		return 1
	}
	if *role != "primary" && *cortexAddr == "" {
		fmt.Fprintln(os.Stderr, "error: --cortex-addr is required for non-primary roles")
		return 1
	}

	if !*yes {
		fmt.Printf("Enable cluster mode on %s? (role=%s, bind=%s) [y/N] ", *addr, *role, *bindAddr)
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("Aborted.")
			return 0
		}
	}

	payload := map[string]any{
		"role":           *role,
		"bind_addr":      *bindAddr,
		"cluster_secret": *secret,
		"cortex_addr":    *cortexAddr,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "internal error: marshal payload: %v\n", err)
		return 1
	}

	body, err := httpPostJSON(*addr+"/api/admin/cluster/enable", data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err == nil {
		if respRole, ok := result["role"].(string); ok {
			fmt.Printf("Cluster mode enabled (role=%s).\n", respRole)
			return 0
		}
	}
	fmt.Println("Cluster mode enabled.")
	return 0
}

// runClusterDisable disables cluster mode on a running node via the admin API.
func runClusterDisable(args []string) int {
	fs := flag.NewFlagSet("cluster disable", flag.ContinueOnError)
	addr := fs.String("addr", "http://127.0.0.1:8475", "MuninnDB admin address")
	yes := fs.Bool("yes", false, "Skip confirmation prompt")

	if err := fs.Parse(args); err != nil {
		return 1
	}
	if !*yes {
		fmt.Print("Disable cluster mode? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("Aborted.")
			return 0
		}
	}

	if _, err := httpPostJSON(*addr+"/api/admin/cluster/disable", nil); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	fmt.Println("Cluster mode disabled.")
	return 0
}

// Helper functions for HTTP calls and JSON parsing

func httpGet(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

func httpPost(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(url, "application/json", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

func httpPostJSON(url string, body []byte) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = bytes.NewReader([]byte{})
	}
	resp, err := client.Post(url, "application/json", bodyReader)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func getString(data map[string]interface{}, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func getBool(data map[string]interface{}, key string) bool {
	if v, ok := data[key].(bool); ok {
		return v
	}
	return false
}

func getUint64(data map[string]interface{}, key string) uint64 {
	if v, ok := data[key].(float64); ok {
		return uint64(v)
	}
	return 0
}

func getMembers(data map[string]interface{}, key string) []map[string]interface{} {
	if members, ok := data[key].([]interface{}); ok {
		var result []map[string]interface{}
		for _, m := range members {
			if mmap, ok := m.(map[string]interface{}); ok {
				result = append(result, mmap)
			}
		}
		return result
	}
	return nil
}
