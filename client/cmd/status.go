package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	yaml2 "gopkg.in/yaml.v2"
	"net"
	"net/netip"
	"sort"
	"strings"

	"github.com/netbirdio/netbird/client/internal"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/proto"
	nbStatus "github.com/netbirdio/netbird/client/status"
	"github.com/netbirdio/netbird/client/system"
	"github.com/netbirdio/netbird/util"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/status"
)

var (
	detailFlag   bool
	ipv4Flag     bool
	jsonFlag     bool
	yamlFlag     bool
	ipsFilter    []string
	statusFilter string
	ipsFilterMap map[string]struct{}
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "status of the Netbird Service",
	RunE:  statusFunc,
}

func init() {
	ipsFilterMap = make(map[string]struct{})
	statusCmd.PersistentFlags().BoolVarP(&detailFlag, "detail", "d", false, "display detailed status information in human-readable format")
	statusCmd.PersistentFlags().BoolVar(&jsonFlag, "json", false, "display detailed status information in json format")
	statusCmd.PersistentFlags().BoolVar(&yamlFlag, "yaml", false, "display detailed status information in yaml format")
	statusCmd.PersistentFlags().BoolVar(&ipv4Flag, "ipv4", false, "display only NetBird IPv4 of this peer, e.g., --ipv4 will output 100.64.0.33")
	statusCmd.MarkFlagsMutuallyExclusive("detail", "json", "yaml", "ipv4")
	statusCmd.PersistentFlags().StringSliceVar(&ipsFilter, "filter-by-ips", []string{}, "filters the detailed output by a list of one or more IPs, e.g., --filter-by-ips 100.64.0.100,100.64.0.200")
	statusCmd.PersistentFlags().StringVar(&statusFilter, "filter-by-status", "", "filters the detailed output by connection status(connected|disconnected), e.g., --filter-by-status connected")
}

func statusFunc(cmd *cobra.Command, args []string) error {
	SetFlagsFromEnvVars(rootCmd)

	cmd.SetOut(cmd.OutOrStdout())

	err := parseFilters()
	if err != nil {
		return err
	}

	err = util.InitLog(logLevel, "console")
	if err != nil {
		return fmt.Errorf("failed initializing log %v", err)
	}

	ctx := internal.CtxInitState(context.Background())

	conn, err := DialClientGRPCServer(ctx, daemonAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to daemon error: %v\n"+
			"If the daemon is not running please run: "+
			"\nnetbird service install \nnetbird service start\n", err)
	}
	defer conn.Close()

	resp, err := proto.NewDaemonServiceClient(conn).Status(cmd.Context(), &proto.StatusRequest{GetFullPeerStatus: true})
	if err != nil {
		return fmt.Errorf("status failed: %v", status.Convert(err).Message())
	}

	daemonStatus := fmt.Sprintf("Daemon status: %s\n", resp.GetStatus())
	if resp.GetStatus() == string(internal.StatusNeedsLogin) || resp.GetStatus() == string(internal.StatusLoginFailed) {

		cmd.Printf("%s\n"+
			"Run UP command to log in with SSO (interactive login):\n\n"+
			" netbird up \n\n"+
			"If you are running a self-hosted version and no SSO provider has been configured in your Management Server,\n"+
			"you can use a setup-key:\n\n netbird up --management-url <YOUR_MANAGEMENT_URL> --setup-key <YOUR_SETUP_KEY>\n\n"+
			"More info: https://www.netbird.io/docs/overview/setup-keys\n\n",
			daemonStatus,
		)
		return nil
	}

	pbFullStatus := resp.GetFullStatus()
	fullStatus := fromProtoFullStatus(pbFullStatus)

	statusOutputString := ""
	if detailFlag {
		statusOutputString = parseToHumanReadable(fullStatus, daemonStatus, resp.GetDaemonVersion())
	}
	if jsonFlag {
		statusOutputString, err = parseToJson(fullStatus)
		if err != nil {
			return fmt.Errorf("json marshal failed")
		}
	}
	if yamlFlag {
		statusOutputString, err = parseToYaml(fullStatus)
		if err != nil {
			return fmt.Errorf("yaml marshal failed")
		}
	}
	if ipv4Flag {
		statusOutputString = parseInterfaceIP(fullStatus.LocalPeerState.IP)
	}

	cmd.Print(statusOutputString)

	return nil
}

func parseFilters() error {
	switch strings.ToLower(statusFilter) {
	case "", "disconnected", "connected":
	default:
		return fmt.Errorf("wrong status filter, should be one of connected|disconnected, got: %s", statusFilter)
	}

	if len(ipsFilter) > 0 {
		for _, addr := range ipsFilter {
			_, err := netip.ParseAddr(addr)
			if err != nil {
				return fmt.Errorf("got an invalid IP address in the filter: address %s, error %s", addr, err)
			}
			ipsFilterMap[addr] = struct{}{}
		}
	}
	return nil
}

func fromProtoFullStatus(pbFullStatus *proto.FullStatus) nbStatus.FullStatus {
	var fullStatus nbStatus.FullStatus
	managementState := pbFullStatus.GetManagementState()
	fullStatus.ManagementState.URL = managementState.GetURL()
	fullStatus.ManagementState.Connected = managementState.GetConnected()

	signalState := pbFullStatus.GetSignalState()
	fullStatus.SignalState.URL = signalState.GetURL()
	fullStatus.SignalState.Connected = signalState.GetConnected()

	localPeerState := pbFullStatus.GetLocalPeerState()
	fullStatus.LocalPeerState.IP = localPeerState.GetIP()
	fullStatus.LocalPeerState.PubKey = localPeerState.GetPubKey()
	fullStatus.LocalPeerState.KernelInterface = localPeerState.GetKernelInterface()
	fullStatus.LocalPeerState.FQDN = localPeerState.GetFqdn()

	var peersState []nbStatus.PeerState

	for _, pbPeerState := range pbFullStatus.GetPeers() {
		timeLocal := pbPeerState.GetConnStatusUpdate().AsTime().Local()
		peerState := nbStatus.PeerState{
			IP:                     pbPeerState.GetIP(),
			PubKey:                 pbPeerState.GetPubKey(),
			ConnStatus:             pbPeerState.GetConnStatus(),
			ConnStatusUpdate:       timeLocal,
			Relayed:                pbPeerState.GetRelayed(),
			Direct:                 pbPeerState.GetDirect(),
			LocalIceCandidateType:  pbPeerState.GetLocalIceCandidateType(),
			RemoteIceCandidateType: pbPeerState.GetRemoteIceCandidateType(),
			FQDN:                   pbPeerState.GetFqdn(),
		}
		peersState = append(peersState, peerState)
	}

	fullStatus.Peers = peersState

	return fullStatus
}

func parseInterfaceIP(interfaceIP string) string {
	ip, _, err := net.ParseCIDR(interfaceIP)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s\n", ip)
}

func parseToJson(fullStatus nbStatus.FullStatus) (string, error) {
	jsonBytes, err := json.Marshal(fullStatus)
	return string(jsonBytes), err
}

func parseToYaml(fullStatus nbStatus.FullStatus) (string, error) {
	yamlBytes, err := yaml2.Marshal(fullStatus)
	return string(yamlBytes), err
}

func countConnectedPeers(peers []nbStatus.PeerState) int {
	peersConnected := 0
	for _, peerState := range peers {
		if peerState.ConnStatus == peer.StatusConnected.String() {
			peersConnected = peersConnected + 1
		}
	}
	return peersConnected
}

func parseGeneralSummary(fullStatus nbStatus.FullStatus, daemonStatus string, daemonVersion string) string {

	managementStatusURL := fmt.Sprintf(" to %s", fullStatus.ManagementState.URL)
	signalStatusURL := fmt.Sprintf(" to %s", fullStatus.SignalState.URL)

	managementConnString := "Disconnected"
	if fullStatus.ManagementState.Connected {
		managementConnString = "Connected"
	}

	signalConnString := "Disconnected"
	if fullStatus.SignalState.Connected {
		signalConnString = "Connected"
	}

	interfaceTypeString := "Userspace"
	interfaceIP := ""
	if fullStatus.LocalPeerState.KernelInterface {
		interfaceTypeString = "Kernel"
	} else if fullStatus.LocalPeerState.IP == "" {
		interfaceTypeString = "N/A"
		interfaceIP = "N/A"
	}

	peersConnected := countConnectedPeers(fullStatus.Peers)
	peersCountString := fmt.Sprintf("%d/%d Connected", peersConnected, len(fullStatus.Peers))

	summary := fmt.Sprintf(
		"Daemon version: %s\n"+
			"CLI version: %s\n"+
			"%s"+ // daemon status
			"Management: %s%s\n"+
			"Signal: %s%s\n"+
			"Domain: %s\n"+
			"NetBird IP: %s\n"+
			"Interface type: %s\n"+
			"Peers count: %s\n",
		daemonVersion,
		system.NetbirdVersion(),
		daemonStatus,
		managementConnString,
		managementStatusURL,
		signalConnString,
		signalStatusURL,
		fullStatus.LocalPeerState.FQDN,
		interfaceIP,
		interfaceTypeString,
		peersCountString,
	)
	return summary
}

func parseToHumanReadable(fullStatus nbStatus.FullStatus, daemonStatus string, daemonVersion string) string {
	parsedPeersString := parsePeers(fullStatus.Peers)
	summary := parseGeneralSummary(fullStatus, daemonStatus, daemonVersion)

	return fmt.Sprintf(
		"Peers detail:"+
			"%s\n"+
			"%s",
		parsedPeersString,
		summary,
	)
}

func parsePeers(peers []nbStatus.PeerState) string {
	var (
		peersString = ""
	)

	if len(peers) > 0 {
		sort.SliceStable(peers, func(i, j int) bool {
			iAddr, _ := netip.ParseAddr(peers[i].IP)
			jAddr, _ := netip.ParseAddr(peers[j].IP)
			return iAddr.Compare(jAddr) == -1
		})
	}

	for _, peerState := range peers {
		peerConnectionStatus := peerState.ConnStatus == peer.StatusConnected.String()
		if skipDetailByFilters(peerState, peerConnectionStatus) {
			continue
		}

		localICE := "-"
		remoteICE := "-"
		connType := "-"

		if peerConnectionStatus {
			localICE = peerState.LocalIceCandidateType
			remoteICE = peerState.RemoteIceCandidateType
			connType = "P2P"
			if peerState.Relayed {
				connType = "Relayed"
			}
		}

		peerString := fmt.Sprintf(
			"\n %s:\n"+
				"  NetBird IP: %s\n"+
				"  Public key: %s\n"+
				"  Status: %s\n"+
				"  -- detail --\n"+
				"  Connection type: %s\n"+
				"  Direct: %t\n"+
				"  ICE candidate (Local/Remote): %s/%s\n"+
				"  Last connection update: %s\n",
			peerState.FQDN,
			peerState.IP,
			peerState.PubKey,
			peerState.ConnStatus,
			connType,
			peerState.Direct,
			localICE,
			remoteICE,
			peerState.ConnStatusUpdate.Format("2006-01-02 15:04:05"),
		)

		peersString = peersString + peerString
	}
	return peersString
}

func skipDetailByFilters(peerState nbStatus.PeerState, isConnected bool) bool {
	statusEval := false
	ipEval := false

	if statusFilter != "" {
		lowerStatusFilter := strings.ToLower(statusFilter)
		if lowerStatusFilter == "disconnected" && isConnected {
			statusEval = true
		} else if lowerStatusFilter == "connected" && !isConnected {
			statusEval = true
		}
	}

	if len(ipsFilter) > 0 {
		_, ok := ipsFilterMap[peerState.IP]
		if !ok {
			ipEval = true
		}
	}
	return statusEval || ipEval
}
