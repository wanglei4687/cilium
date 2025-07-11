// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cilium/cilium/api/v1/client/daemon"
	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/bpf"
	"github.com/cilium/cilium/pkg/defaults"
	endpointid "github.com/cilium/cilium/pkg/endpoint/id"
	"github.com/cilium/cilium/pkg/iana"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/maps/policymap"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/policy/trafficdirection"
	policyTypes "github.com/cilium/cilium/pkg/policy/types"
	"github.com/cilium/cilium/pkg/u8proto"
)

// Fatalf prints the Printf formatted message to stderr and exits the program
// Note: os.Exit(1) is not recoverable
func Fatalf(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: %s\n", fmt.Sprintf(msg, args...))
	os.Exit(1)
}

// Usagef prints the Printf formatted message to stderr, prints usage help and
// exits the program
// Note: os.Exit(1) is not recoverable
func Usagef(cmd *cobra.Command, msg string, args ...any) {
	txt := fmt.Sprintf(msg, args...)
	fmt.Fprintf(os.Stderr, "Error: %s\n\n", txt)
	cmd.Help()
	os.Exit(1)
}

func requireEndpointID(cmd *cobra.Command, args []string) {
	if len(args) < 1 {
		Usagef(cmd, "Missing endpoint id argument")
	}

	if id := identity.GetReservedID(args[0]); id == identity.IdentityUnknown {
		_, _, err := endpointid.Parse(args[0])

		if err != nil {
			Fatalf("Cannot parse endpoint id \"%s\": %s", args[0], err)
		}
	}
}

func requireRecorderID(cmd *cobra.Command, args []string) {
	if len(args) < 1 {
		Usagef(cmd, "Missing recorder id argument")
	}

	if args[0] == "" {
		Usagef(cmd, "Empty recorder id argument")
	}
}

func requirePath(cmd *cobra.Command, args []string) {
	if len(args) < 1 {
		Usagef(cmd, "Missing path argument")
	}

	if args[0] == "" {
		Usagef(cmd, "Empty path argument")
	}
}

// TablePrinter prints the map[string][]string, which is an usual representation
// of dumped BPF map, using tabwriter.
func TablePrinter(firstTitle, secondTitle string, data map[string][]string) {
	w := tabwriter.NewWriter(os.Stdout, 5, 0, 3, ' ', 0)

	fmt.Fprintf(w, "%s\t%s\n", firstTitle, secondTitle)

	for key, value := range data {
		for k, v := range value {
			if k == 0 {
				fmt.Fprintf(w, "%s\t%s\t\n", key, v)
			} else {
				fmt.Fprintf(w, "%s\t%s\t\n", "", v)
			}
		}
	}

	w.Flush()
}

// Search 'result' for strings with escaped JSON inside, and expand the JSON.
func expandNestedJSON(result bytes.Buffer) (bytes.Buffer, error) {
	reStringWithJSON := regexp.MustCompile(`"[^"\\{]*{.*[^\\]"`)
	reJSON := regexp.MustCompile(`{(.|\n)*}`)
	for {
		var (
			loc    []int
			indent string
		)

		// Search for nested JSON; if we don't find any, then break.
		resBytes := result.Bytes()
		if loc = reStringWithJSON.FindIndex(resBytes); loc == nil {
			break
		}

		// Determine the current indentation
		for i := range loc[0] - 1 {
			idx := loc[0] - i - 1
			if resBytes[idx] != ' ' {
				break
			}
			indent = fmt.Sprintf("\t%s\t", indent)
		}

		stringStart := loc[0]
		stringEnd := loc[1]

		// Unquote the string with the nested json.
		quotedBytes := resBytes[stringStart:stringEnd]
		unquoted, err := strconv.Unquote(string(quotedBytes))
		if err != nil {
			return bytes.Buffer{}, fmt.Errorf("Failed to Unquote string: %s\n%s", err.Error(), string(quotedBytes))
		}

		// Find the JSON within the unquoted string.
		nestedStart := 0
		nestedEnd := 0
		// Find the left-most match
		if loc = reJSON.FindStringIndex(unquoted); loc != nil {
			nestedStart = loc[0]
			nestedEnd = loc[1]
		}

		// Decode the nested JSON
		decoded := ""
		if nestedEnd != 0 {
			m := make(map[string]any)
			nested := bytes.NewBufferString(unquoted[nestedStart:nestedEnd])
			if err := json.NewDecoder(nested).Decode(&m); err != nil {
				return bytes.Buffer{}, fmt.Errorf("Failed to decode nested JSON: %s (\n%s\n)", err.Error(), unquoted[nestedStart:nestedEnd])
			}
			decodedBytes, err := json.MarshalIndent(m, indent, "  ")
			if err != nil {
				return bytes.Buffer{}, fmt.Errorf("Cannot marshal nested JSON: %s", err.Error())
			}
			decoded = string(decodedBytes)
		}

		// Serialize
		nextResult := bytes.Buffer{}
		nextResult.Write(resBytes[0:stringStart])
		nextResult.WriteString(string(unquoted[:nestedStart]))
		nextResult.WriteString(string(decoded))
		nextResult.WriteString(string(unquoted[nestedEnd:]))
		nextResult.Write(resBytes[stringEnd:])
		result = nextResult
	}

	return result, nil
}

// PolicyUpdateArgs is the parsed representation of a
// bpf policy {add,delete} command.
type PolicyUpdateArgs struct {
	// path is the basename of the BPF map for this policy update.
	path string

	// trafficDirection represents the traffic direction provided
	// as an argument e.g. `ingress`
	trafficDirection trafficdirection.TrafficDirection

	// label represents the identity of the label provided as argument.
	label identity.NumericIdentity

	// port represents the port associated with the command, if specified.
	port uint16

	// protocols represents the set of protocols associated with the
	// command, if specified.
	protocols []u8proto.U8proto

	isDeny bool
}

// parseTrafficString converts the provided string to its corresponding
// TrafficDirection. If the string does not correspond to a valid TrafficDirection
// type, returns Invalid and a corresponding error.
func parseTrafficString(td string) (trafficdirection.TrafficDirection, error) {
	lowered := strings.ToLower(td)

	switch lowered {
	case "ingress":
		return trafficdirection.Ingress, nil
	case "egress":
		return trafficdirection.Egress, nil
	default:
		return trafficdirection.Invalid, fmt.Errorf("invalid direction %q provided", td)
	}
}

// parsePolicyUpdateArgs parses the arguments to a bpf policy {add,delete}
// command, provided as a list containing the endpoint ID, traffic direction,
// identity and optionally, a list of ports.
// Returns a parsed representation of the command arguments.
func parsePolicyUpdateArgs(logger *slog.Logger, cmd *cobra.Command, args []string, isDeny bool) *PolicyUpdateArgs {
	if len(args) < 3 {
		Usagef(cmd, "<endpoint id>, <traffic-direction>, and <identity> required")
	}

	pa, err := parsePolicyUpdateArgsHelper(logger, args, isDeny)
	if err != nil {
		Fatalf("%s", err)
	}

	return pa
}

func endpointToPolicyMapPath(logger *slog.Logger, endpointID string) (string, error) {
	if endpointID == "" {
		return "", fmt.Errorf("Need ID or label")
	}

	var mapName string
	idUint64, err := strconv.ParseUint(endpointID, 10, 16)
	if err == nil {
		mapName = bpf.LocalMapName(policymap.MapName, uint16(idUint64))
	} else if numericIdentity := identity.GetReservedID(endpointID); numericIdentity != identity.IdentityUnknown {
		mapSuffix := "reserved_" + strconv.FormatUint(uint64(numericIdentity), 10)
		mapName = fmt.Sprintf("%s%s", policymap.MapName, mapSuffix)
	} else {
		return "", err
	}

	return bpf.MapPath(logger, mapName), nil
}

func parsePolicyUpdateArgsHelper(logger *slog.Logger, args []string, isDeny bool) (*PolicyUpdateArgs, error) {
	trafficDirection := args[1]
	parsedTd, err := parseTrafficString(trafficDirection)
	if err != nil {
		return nil, fmt.Errorf("Failed to convert %s to a valid traffic direction: %w", args[1], err)
	}

	mapName, err := endpointToPolicyMapPath(logger, args[0])
	if err != nil {
		return nil, fmt.Errorf("Failed to parse endpointID %q", args[0])
	}

	peerLbl, err := strconv.ParseUint(args[2], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("Failed to convert %s", args[2])
	}
	label := identity.NumericIdentity(peerLbl)

	port := uint16(0)
	protos := []u8proto.U8proto{}
	if len(args) > 3 {
		pp, err := parseL4PortsSlice([]string{args[3]})
		if err != nil {
			return nil, fmt.Errorf("Failed to parse L4: %w", err)
		}
		port = pp[0].Port
		if port != 0 {
			proto, _ := u8proto.ParseProtocol(pp[0].Protocol)
			if proto == 0 {
				for _, proto := range u8proto.ProtoIDs {
					protos = append(protos, proto)
				}
			} else {
				protos = append(protos, proto)
			}
		}
	}
	if len(protos) == 0 {
		protos = append(protos, 0)
	}

	pa := &PolicyUpdateArgs{
		path:             mapName,
		trafficDirection: parsedTd,
		label:            label,
		port:             port,
		protocols:        protos,
		isDeny:           isDeny,
	}

	return pa, nil
}

// updatePolicyKey updates an entry in the PolicyMap for the provided
// PolicyUpdateArgs argument.
// Adds the entry to the PolicyMap if add is true, otherwise the entry is
// deleted.
func updatePolicyKey(pa *PolicyUpdateArgs, add bool) {
	// The map needs not to be transparently initialized here even if
	// it's not present for some reason. Triggering map recreation with
	// OpenOrCreate when some map attribute had changed would be much worse.
	policyMap, err := policymap.OpenPolicyMap(log, pa.path)
	if err != nil {
		Fatalf("Cannot open policymap %q : %s", pa.path, err)
	}

	for _, proto := range pa.protocols {
		u8p := u8proto.U8proto(proto)
		entry := fmt.Sprintf("%d %d/%s", pa.label, pa.port, u8p.String())
		mapKey := policymap.NewKeyFromPolicyKey(policyTypes.KeyForDirection(pa.trafficDirection).WithIdentity(pa.label).WithPortProto(proto, pa.port))
		if add {
			mapEntry := policymap.NewEntryFromPolicyEntry(mapKey, policyTypes.MapStateEntry{}.WithDeny(pa.isDeny))
			if err := policyMap.Update(&mapKey, &mapEntry); err != nil {
				Fatalf("Cannot add policy key '%s': %s\n", entry, err)
			}
		} else {
			if err := policyMap.DeleteKey(mapKey); err != nil {
				Fatalf("Cannot delete policy key '%s': %s\n", entry, err)
			}
		}
	}
}

// dumpConfig pretty prints boolean options
func dumpConfig(Opts map[string]string, indented bool) {
	for _, k := range slices.Sorted(maps.Keys(Opts)) {
		// XXX: Reuse the format function from *option.Library
		value = Opts[k]
		formatStr := "%-34s: %s\n"
		if indented {
			formatStr = "\t%-26s: %s\n"
		}
		if enabled, err := option.NormalizeBool(value); err != nil {
			// If it cannot be parsed as a bool, just format the value.
			fmt.Printf(formatStr, k, value)
		} else if enabled == option.OptionDisabled {
			fmt.Printf(formatStr, k, "Disabled")
		} else {
			fmt.Printf(formatStr, k, "Enabled")
		}
	}
}

func mapKeysToLowerCase(s map[string]any) map[string]any {
	m := make(map[string]any)
	for k, v := range s {
		if reflect.ValueOf(v).Kind() == reflect.Map {
			for i, j := range v.(map[string]any) {
				m[strings.ToLower(i)] = j
			}
		}
		m[strings.ToLower(k)] = v
	}
	return m
}

// getIpEnableStatuses api returns the EnableIPv6 and EnableIPv4 statuses by
// consulting the cilium-agent otherwise reads from the runtime system config.
func getIpEnableStatuses() (bool, bool) {
	params := daemon.NewGetHealthzParamsWithTimeout(5 * time.Second)
	brief := true
	params.SetBrief(&brief)
	// If cilium-agent is running get the enable statuses
	if _, err := client.Daemon.GetHealthz(params); err == nil {
		if resp, err := client.ConfigGet(); err == nil {
			if resp.Status != nil {
				ipv4 := resp.Status.Addressing.IPV4 != nil && resp.Status.Addressing.IPV4.Enabled
				ipv6 := resp.Status.Addressing.IPV6 != nil && resp.Status.Addressing.IPV6.Enabled
				return ipv4, ipv6
			}
		}
	} else { // else read the statuses from the file-system
		agentConfigFile := filepath.Join(defaults.RuntimePath, defaults.StateDir,
			"agent-runtime-config.json")

		if byteValue, err := os.ReadFile(agentConfigFile); err == nil {
			if err = json.Unmarshal(byteValue, &option.Config); err == nil {
				return option.Config.EnableIPv4, option.Config.EnableIPv6
			}
		}
	}
	// returning the default statuses
	return defaults.EnableIPv4, defaults.EnableIPv6
}

func mergeMaps(m1, m2 map[string]any) map[string]any {
	m3 := maps.Clone(m1)
	maps.Copy(m3, m2)
	return m3
}

// parseL4PortsSlice parses a given `slice` of strings. Each string should be in
// the form of `<port>[/<protocol>]`, where the `<port>` is an integer or a port name and
// `<protocol>` is an optional layer 4 protocol `tcp` or `udp`. In case
// `protocol` is not present, or is set to `any`, the parsed port will be set to
// `models.PortProtocolAny`.
func parseL4PortsSlice(slice []string) ([]*models.Port, error) {
	rules := []*models.Port{}
	for _, v := range slice {
		vSplit := strings.Split(v, "/")
		var protoStr string
		switch len(vSplit) {
		case 1:
			protoStr = models.PortProtocolANY
		case 2:
			protoStr = strings.ToUpper(vSplit[1])
			switch protoStr {
			case models.PortProtocolTCP, models.PortProtocolUDP, models.PortProtocolSCTP, models.PortProtocolICMP, models.PortProtocolICMPV6, models.PortProtocolANY:
			default:
				return nil, fmt.Errorf("invalid protocol %q", protoStr)
			}
		default:
			return nil, fmt.Errorf("invalid format %q. Should be <port>[/<protocol>]", v)
		}
		var port uint16
		portStr := vSplit[0]
		if !iana.IsSvcName(portStr) {
			portUint64, err := strconv.ParseUint(portStr, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("invalid port %q: %w", portStr, err)
			}
			port = uint16(portUint64)
			portStr = ""
		}
		l4 := &models.Port{
			Port:     port,
			Name:     portStr,
			Protocol: protoStr,
		}
		rules = append(rules, l4)
	}
	return rules, nil
}
