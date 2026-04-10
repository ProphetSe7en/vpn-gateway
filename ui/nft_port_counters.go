package main

import (
	"os/exec"
	"regexp"
	"strconv"
)

// nftChainOutput caches the raw output of both nft chains for a single
// sample tick, so we run `nft list chain` only twice per tick regardless
// of how many consumers need to parse it (drop counters + port counters).
type nftChainOutput struct {
	input  string
	output string
}

// readNftChains runs `nft list chain` for both input and output chains
// once and returns the raw output. Called outside the mutex since
// exec.Command may take a few ms.
func readNftChains() nftChainOutput {
	var co nftChainOutput
	if out, err := exec.Command("nft", "list", "chain", "inet", "hotio", "input").Output(); err == nil {
		co.input = string(out)
	}
	if out, err := exec.Command("nft", "list", "chain", "inet", "hotio", "output").Output(); err == nil {
		co.output = string(out)
	}
	return co
}

// parseNftDropBytes extracts drop counter bytes from cached chain output.
// Replaces the old readNftDropBytes() which ran its own nft commands.
func parseNftDropBytes(co nftChainOutput) (rxDrop, txDrop uint64) {
	rxDrop = parseNftCounterBytes(co.input, "traffic-limit-down")
	txDrop = parseNftCounterBytes(co.output, "traffic-limit-up")
	return
}

// nftPortRe matches per-port counter rules in nft output. Captures:
//
//	[1] = port number, [2] = bytes count
//
// Works for both input (tcp dport) and output (tcp sport) chains.
// Handles multiple subnet rules per port by matching all occurrences.
var nftPortRe = regexp.MustCompile(`tcp [sd]port (\d+)\b.*?counter packets \d+ bytes (\d+)`)

// parseNftPortBytes sums the byte counters across all nft rules matching
// the given port. For per-service traffic:
//   - output chain (tcp sport <port>): bytes flowing TO LAN clients (stream data)
//   - input chain (tcp dport <port>): bytes flowing FROM LAN clients (control/ACKs)
//
// Multiple subnet rules per port (e.g. 192.168.86.0/24, 192.168.2.0/24)
// are summed together.
func parseNftPortBytes(chainOutput string, port int) uint64 {
	var total uint64
	for _, match := range nftPortRe.FindAllStringSubmatch(chainOutput, -1) {
		p, err := strconv.Atoi(match[1])
		if err != nil || p != port {
			continue
		}
		val, _ := strconv.ParseUint(match[2], 10, 64)
		total += val
	}
	return total
}

// detectNftPorts parses nft output chain rules and returns which ports
// have byte counter rules. Called by syncPortCounters to auto-detect
// which services can use nft counters instead of API polling.
func detectNftPorts(co nftChainOutput) map[int]bool {
	result := make(map[int]bool)
	for _, match := range nftPortRe.FindAllStringSubmatch(co.output, -1) {
		p, err := strconv.Atoi(match[1])
		if err == nil {
			result[p] = true
		}
	}
	return result
}
