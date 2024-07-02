package testcase

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rancher/distros-test-framework/config"
	"github.com/rancher/distros-test-framework/factory"
	"github.com/rancher/distros-test-framework/pkg/aws"
	"github.com/rancher/distros-test-framework/shared"

	. "github.com/onsi/gomega"
)

const (
	agent  = "agent"
	master = "master"
)

func TestUpgradeReplaceNode(cluster *factory.Cluster, version string) {
	if version == "" {
		Expect(version).NotTo(BeEmpty(), "version not sent")
	}

	envErr := config.SetEnv(shared.BasePath() + fmt.Sprintf("/config/%s.tfvars",
		cluster.Config.Product))
	Expect(envErr).NotTo(HaveOccurred(), "error setting env: %s", envErr)

	resourceName := os.Getenv("resource_name")

	awsDependencies, err := aws.AddNode(cluster)
	Expect(err).NotTo(HaveOccurred(), "error adding aws nodes: %s", err)

	var (
		serverNames,
		instanceServerIds,
		newExternalServerIps,
		newPrivateServerIps []string
		createErr error
	)

	// create server names
	for i := 0; i < len(cluster.ServerIPs); i++ {
		serverNames = append(serverNames, fmt.Sprintf("%s-server-replace-%d", resourceName, i+1))
	}

	newExternalServerIps, newPrivateServerIps, instanceServerIds, createErr =
		awsDependencies.CreateInstances(serverNames...)
	Expect(createErr).NotTo(HaveOccurred(), createErr)

	shared.LogLevel("info", "Created server public ips: %s and ids: %s\n",
		newExternalServerIps, instanceServerIds)

	scpErr := scpToNewNodes(cluster, master, newExternalServerIps)
	Expect(scpErr).NotTo(HaveOccurred(), scpErr)
	shared.LogLevel("info", "Scp files to new server nodes done\n")

	serverLeaderIp := cluster.ServerIPs[0]
	token, err := shared.FetchToken(serverLeaderIp)
	Expect(err).NotTo(HaveOccurred(), err)

	serverErr := replaceServers(
		cluster,
		awsDependencies,
		resourceName,
		serverLeaderIp,
		token,
		version,
		newExternalServerIps,
		newPrivateServerIps,
	)
	Expect(serverErr).NotTo(HaveOccurred(), serverErr)

	shared.LogLevel("info", "Server control plane nodes replaced with ips: %s\n", newExternalServerIps)

	// replace agents only if exists
	if len(cluster.AgentIPs) > 0 {
		// create agent names
		var agentNames []string
		for i := 0; i < len(cluster.AgentIPs); i++ {
			agentNames = append(agentNames, fmt.Sprintf("%s-agent-replace-%d", resourceName, i+1))
		}

		newExternalAgentIps, newPrivateAgentIps, instanceAgentIds, createAgentErr :=
			awsDependencies.CreateInstances(agentNames...)
		Expect(createAgentErr).NotTo(HaveOccurred(), createAgentErr)

		shared.LogLevel("info", "created worker ips: %s and worker ids: %s\n",
			newExternalAgentIps, instanceAgentIds)

		scpErr := scpToNewNodes(cluster, agent, newExternalAgentIps)
		Expect(scpErr).NotTo(HaveOccurred(), scpErr)
		shared.LogLevel("info", "Scp files to new worker nodes done\n")

		agentErr := replaceAgents(cluster, awsDependencies, serverLeaderIp, token, version, newExternalAgentIps,
			newPrivateAgentIps,
		)
		Expect(agentErr).NotTo(HaveOccurred(), "error replacing agents: %s", agentErr)

		shared.LogLevel("info", "Agent nodes replaced with ips: %s\n", newExternalAgentIps)
	}

	// delete the last remaining server = leader
	delErr := deleteServer(serverLeaderIp, awsDependencies)
	Expect(delErr).NotTo(HaveOccurred(), delErr)

	shared.LogLevel("info", "Last Server deleted ip: %s\n", serverLeaderIp)
}

func scpToNewNodes(cluster *factory.Cluster, nodeType string, newNodeIps []string) error {
	if newNodeIps == nil {
		return shared.ReturnLogError("newServerIps should send at least one ip\n")
	}

	if cluster.Config.Product != "k3s" && cluster.Config.Product != "rke2" {
		return shared.ReturnLogError("unsupported product: %s\n", cluster.Config.Product)
	}

	chanErr := make(chan error, len(newNodeIps))
	var wg sync.WaitGroup

	for _, ip := range newNodeIps {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			var err error
			if cluster.Config.Product == "k3s" {
				err = scpK3sFiles(cluster, nodeType, ip)
			} else {
				err = scpRke2Files(cluster, nodeType, ip)
			}
			if err != nil {
				chanErr <- shared.ReturnLogError("error scp files to new nodes: %w\n", err)
				close(chanErr)
			}
		}(ip)
	}

	wg.Wait()
	close(chanErr)

	return nil
}

func scpRke2Files(cluster *factory.Cluster, nodeType, ip string) error {
	joinLocalPath := shared.BasePath() + fmt.Sprintf("/modules/install/join_rke2_%s.sh", nodeType)
	joinRemotePath := fmt.Sprintf("/tmp/join_rke2_%s.sh", nodeType)

	if err := shared.RunScp(cluster, ip, []string{joinLocalPath}, []string{joinRemotePath}); err != nil {
		return shared.ReturnLogError("error running scp: %w with ip: %s", err, ip)
	}

	return nil
}

func scpK3sFiles(cluster *factory.Cluster, nodeType, ip string) error {
	if nodeType == agent {
		cisWorkerLocalPath := shared.BasePath() + "/modules/k3s/worker/cis_worker_config.yaml"
		cisWorkerRemotePath := "/tmp/cis_worker_config.yaml"

		joinLocalPath := shared.BasePath() + fmt.Sprintf("/modules/install/join_k3s_%s.sh", agent)
		joinRemotePath := fmt.Sprintf("/tmp/join_k3s_%s.sh", agent)

		if err := shared.RunScp(
			cluster,
			ip,
			[]string{cisWorkerLocalPath, joinLocalPath},
			[]string{cisWorkerRemotePath, joinRemotePath},
		); err != nil {
			return err
		}
	} else {
		cisMasterLocalPath := shared.BasePath() + "/modules/k3s/master/cis_master_config.yaml"
		cisMasterRemotePath := "/tmp/cis_master_config.yaml"

		clusterLevelpssLocalPath := shared.BasePath() + "/modules/k3s/master/cluster-level-pss.yaml"
		clusterLevelpssRemotePath := "/tmp/cluster-level-pss.yaml"

		auditLocalPath := shared.BasePath() + "/modules/k3s/master/audit.yaml"
		auditRemotePath := "/tmp/audit.yaml"

		policyLocalPath := shared.BasePath() + "/modules/k3s/master/policy.yaml"
		policyRemotePath := "/tmp/policy.yaml"

		ingressPolicyLocalPath := shared.BasePath() + "/modules/k3s/master/ingresspolicy.yaml"
		ingressPolicyRemotePath := "/tmp/ingresspolicy.yaml"

		joinLocalPath := shared.BasePath() + fmt.Sprintf("/modules/install/join_k3s_%s.sh", master)
		joinRemotePath := fmt.Sprintf("/tmp/join_k3s_%s.sh", master)

		if err := shared.RunScp(
			cluster,
			ip,
			[]string{
				cisMasterLocalPath,
				clusterLevelpssLocalPath,
				auditLocalPath,
				policyLocalPath,
				ingressPolicyLocalPath,
				joinLocalPath,
			},
			[]string{
				cisMasterRemotePath,
				clusterLevelpssRemotePath,
				auditRemotePath,
				policyRemotePath,
				ingressPolicyRemotePath,
				joinRemotePath,
			}); err != nil {
			return err
		}
	}

	return nil
}

func replaceServers(
	cluster *factory.Cluster,
	a *aws.Client,
	resourceName, serverLeaderIp, token, version string,
	newExternalServerIps, newPrivateServerIps []string,
) error {
	if token == "" {
		return shared.ReturnLogError("token not sent\n")
	}

	if len(newExternalServerIps) == 0 || len(newPrivateServerIps) == 0 {
		return shared.ReturnLogError("externalIps or privateIps empty\n")
	}

	// join the first new server
	newFirstServerIP := newExternalServerIps[0]
	err := serverJoin(cluster, serverLeaderIp, token, version, newFirstServerIP, newPrivateServerIps[0])
	if err != nil {
		shared.LogLevel("error", "error joining first server: %w\n", err)

		return err
	}
	shared.LogLevel("info", "Proceeding to update config file after first server join %s\n", newFirstServerIP)

	// delete first the server that is not the leader neither the server ip in the kubeconfig
	oldServerIPs := cluster.ServerIPs
	if delErr := deleteServer(oldServerIPs[len(oldServerIPs)-2], a); delErr != nil {
		shared.LogLevel("error", "error deleting server: %w\n", delErr)

		return delErr
	}

	// update the kubeconfig file to point to the new added server
	if kbCfgErr := shared.UpdateKubeConfig(newFirstServerIP, resourceName, cluster.Config.Product); kbCfgErr != nil {
		return shared.ReturnLogError("error updating kubeconfig: %w with ip: %s", kbCfgErr, newFirstServerIP)
	}
	shared.LogLevel("info", "Updated local kubeconfig with ip: %s", newFirstServerIP)

	nodeErr := validateNodeJoin(newFirstServerIP)
	if nodeErr != nil {
		shared.LogLevel("error", "error validating node join: %w with ip: %s", nodeErr, newFirstServerIP)

		return nodeErr
	}

	// join the rest of the servers and delete all except the leader
	for i := 1; i <= len(newExternalServerIps[1:]); i++ {
		privateIp := newPrivateServerIps[i]
		externalIp := newExternalServerIps[i]

		if i < len(oldServerIPs[1:]) {
			if delErr := deleteServer(oldServerIPs[len(oldServerIPs)-1], a); delErr != nil {
				shared.LogLevel("error", "error deleting server: %w\n for ip: %s", delErr, oldServerIPs[i])

				return delErr
			}
		}

		if joinErr := serverJoin(cluster, serverLeaderIp, token, version, externalIp, privateIp); joinErr != nil {
			shared.LogLevel("error", "error joining server: %w with ip: %s\n", joinErr, externalIp)

			return joinErr
		}

		nodeErr = validateNodeJoin(externalIp)
		if nodeErr != nil {
			shared.LogLevel("error", "error validating node join: %w with ip: %s", nodeErr, externalIp)

			return nodeErr
		}
	}

	return nil
}

func validateNodeJoin(ip string) error {
	node, err := shared.GetNodeNameByIP(ip)
	if err != nil {
		return shared.ReturnLogError("error getting node name by ip:%s %w\n", ip, err)
	}
	if node == "" {
		return shared.ReturnLogError("node not found\n")
	}
	node = strings.TrimSpace(node)

	shared.LogLevel("info", "Node joined: %s with ip: %s", node, ip)

	return nil
}

func serverJoin(cluster *factory.Cluster, serverLeaderIP, token, version, newExternalIP, newPrivateIP string) error {

	joinCmd, parseErr := buildJoinCmd(cluster, master, serverLeaderIP, token, version, newExternalIP, newPrivateIP)
	if parseErr != nil {
		return shared.ReturnLogError("error parsing join commands: %w\n", parseErr)
	}

	if joinErr := joinNode(joinCmd, newExternalIP); joinErr != nil {
		return shared.ReturnLogError("error joining node: %w\n", joinErr)
	}

	return nil
}

func deleteServer(ip string, a *aws.Client) error {
	if ip == "" {
		return shared.ReturnLogError("ip not sent\n")
	}

	if delNodeErr := shared.DeleteNode(ip); delNodeErr != nil {
		shared.LogLevel("error", "error deleting server: %w\n", delNodeErr)

		return delNodeErr
	}
	shared.LogLevel("info", "Node IP deleted from the cluster: %s\n", ip)

	err := a.DeleteInstance(ip)
	if err != nil {
		return err
	}

	return nil
}

func replaceAgents(
	cluster *factory.Cluster,
	a *aws.Client,
	serverLeaderIp, token, version string,
	newExternalAgentIps, newPrivateAgentIps []string,
) error {
	if token == "" {
		return shared.ReturnLogError("token not sent\n")
	}

	if len(newExternalAgentIps) == 0 || len(newPrivateAgentIps) == 0 {
		return shared.ReturnLogError("externalIps or privateIps empty\n")
	}

	if err := deleteAgents(a, cluster); err != nil {
		shared.LogLevel("error", "error deleting agent: %w\n", err)

		return err
	}

	for i, externalIp := range newExternalAgentIps {
		privateIp := newPrivateAgentIps[i]

		joinErr := joinAgent(cluster, serverLeaderIp, token, version, externalIp, privateIp)
		if joinErr != nil {
			shared.LogLevel("error", "error joining agent: %w\n", joinErr)

			return joinErr
		}
	}

	return nil
}

func deleteAgents(a *aws.Client, c *factory.Cluster) error {
	for _, i := range c.AgentIPs {
		if deleteNodeErr := shared.DeleteNode(i); deleteNodeErr != nil {
			shared.LogLevel("error", "error deleting agent: %w\n", deleteNodeErr)

			return deleteNodeErr
		}
		shared.LogLevel("info", "Node IP deleted from the cluster: %s\n", i)

		err := a.DeleteInstance(i)
		if err != nil {
			return err
		}
		shared.LogLevel("info", "Instance IP deleted from cloud provider: %s\n", i)
	}

	return nil
}

func joinAgent(cluster *factory.Cluster, serverIp, token, version, selfExternalIp, selfPrivateIp string) error {
	cmd, parseErr := buildJoinCmd(cluster, agent, serverIp, token, version, selfExternalIp, selfPrivateIp)
	if parseErr != nil {
		return shared.ReturnLogError("error parsing join commands: %w\n", parseErr)
	}

	if joinErr := joinNode(cmd, selfExternalIp); joinErr != nil {
		return shared.ReturnLogError("error joining node: %w\n", joinErr)
	}

	return nil
}

func joinNode(cmd, ip string) error {
	if cmd == "" {
		return shared.ReturnLogError("cmd not sent\n")
	}
	if ip == "" {
		return shared.ReturnLogError("server IP not sent\n")
	}

	res, err := shared.RunCommandOnNode(cmd, ip)
	if err != nil {
		return shared.ReturnLogError("error joining node: %w\n", err)
	}
	res = strings.TrimSpace(res)
	if strings.Contains(res, "service failed") {
		shared.LogLevel("error", "join node response: %s\n", res)

		return shared.ReturnLogError("error joining node: %s\n", res)
	}

	delay := time.After(40 * time.Second)
	// delay not meant to wait if node is joined, but rather to give time for all join process to complete under the hood
	<-delay

	return nil
}

func buildJoinCmd(
	cluster *factory.Cluster,
	nodetype, serverIp, token, version, selfExternalIp, selfPrivateIp string,
) (string, error) {
	if nodetype != master && nodetype != agent {
		return "", shared.ReturnLogError("unsupported nodetype: %s\n", nodetype)
	}

	var flags string
	var installMode string
	if nodetype == master {
		flags = fmt.Sprintf("'%s'", os.Getenv("server_flags"))
	} else {
		flags = fmt.Sprintf("'%s'", os.Getenv("worker_flags"))
	}

	if strings.HasPrefix(version, "v") {
		installMode = fmt.Sprintf("INSTALL_%s_VERSION", strings.ToUpper(cluster.Config.Product))
	} else {
		installMode = fmt.Sprintf("INSTALL_%s_COMMIT", strings.ToUpper(cluster.Config.Product))
	}

	switch cluster.Config.Product {
	case "k3s":
		return buildK3sCmd(cluster, nodetype, serverIp, token, version, selfExternalIp, selfPrivateIp, installMode, flags)
	case "rke2":
		return buildRke2Cmd(cluster, nodetype, serverIp, token, version, selfExternalIp, selfPrivateIp, installMode, flags)
	default:
		return "", shared.ReturnLogError("unsupported product: %s\n", cluster.Config.Product)
	}
}

func buildK3sCmd(
	cluster *factory.Cluster,
	nodetype, serverIp, token, version, selfExternalIp, selfPrivateIp, instalMode, flags string,
) (string, error) {
	var cmd string
	ipv6 := ""
	if nodetype == agent {
		cmd = fmt.Sprintf(
			"sudo /tmp/join_k3s_%s.sh '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' %s '%s' '%s'",
			nodetype,
			os.Getenv("node_os"),
			serverIp,
			token,
			selfExternalIp,
			selfPrivateIp,
			ipv6,
			instalMode,
			version,
			os.Getenv("k3s_channel"),
			flags,
			os.Getenv("username"),
			os.Getenv("password"),
		)
	} else {
		datastoreEndpoint := cluster.Config.RenderedTemplate
		cmd = fmt.Sprintf(
			"sudo /tmp/join_k3s_%s.sh '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' %s '%s' '%s'",
			nodetype,
			os.Getenv("node_os"),
			serverIp,
			serverIp,
			token,
			selfExternalIp,
			selfPrivateIp,
			ipv6,
			instalMode,
			version,
			os.Getenv("k3s_channel"),
			os.Getenv("datastore_type"),
			datastoreEndpoint,
			flags,
			os.Getenv("username"),
			os.Getenv("password"),
		)
	}

	return cmd, nil
}

func buildRke2Cmd(
	cluster *factory.Cluster,
	nodetype, serverIp, token, version, selfExternalIp, selfPrivateIp, instalMode, flags string,
) (string, error) {
	installMethod := os.Getenv("install_method")
	var cmd string
	ipv6 := ""
	if nodetype == agent {
		cmd = fmt.Sprintf(
			"sudo /tmp/join_rke2_%s.sh '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' %s '%s' '%s'",
			nodetype,
			os.Getenv("node_os"),
			serverIp,
			token,
			selfExternalIp,
			selfPrivateIp,
			ipv6,
			instalMode,
			version,
			os.Getenv("rke2_channel"),
			installMethod,
			flags,
			os.Getenv("username"),
			os.Getenv("password"),
		)
	} else {
		datastoreEndpoint := cluster.Config.RenderedTemplate
		cmd = fmt.Sprintf(
			"sudo /tmp/join_rke2_%s.sh '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' '%s' %s '%s' '%s'",
			nodetype,
			os.Getenv("node_os"),
			serverIp,
			serverIp,
			token,
			selfExternalIp,
			selfPrivateIp,
			ipv6,
			instalMode,
			version,
			os.Getenv("rke2_channel"),
			installMethod,
			os.Getenv("datastore_type"),
			datastoreEndpoint,
			flags,
			os.Getenv("username"),
			os.Getenv("password"),
		)
	}

	return cmd, nil
}
