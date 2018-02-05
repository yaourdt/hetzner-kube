package cmd

import (
	"fmt"
	"github.com/hetznercloud/hcloud-go/hcloud"
	"strings"
	"log"
	"sync"
)

func (cluster *Cluster) InstallWorkers() error {
	var joinCommand string
	// find master
	for _, node := range cluster.Nodes {
		if node.IsMaster {
			output, err := runCmd(node, "kubeadm token create --print-join-command")
			if err != nil {
				return err
			}
			joinCommand = output
			break
		}
	}

	// now let the nodes join

	for _, node := range cluster.Nodes {
		if !node.IsMaster {
			cluster.coordinator.AddEvent(node.Name, "registering node")
			_, err := runCmd(node, "swapoff -a && "+joinCommand)
			if err != nil {
				return err
			}

			cluster.coordinator.AddEvent(node.Name, "complete!")
		}
	}

	return nil
}

func (cluster *Cluster) CreateNodes(suffix string, template Node, count int) error {
	sshKey, _, err := AppConf.Client.SSHKey.Get(AppConf.Context, template.SSHKeyName)

	if err != nil {
		return err
	}

	serverNameTemplate := fmt.Sprintf("%s-%s-@idx", cluster.Name, suffix)
	serverOptsTemplate := hcloud.ServerCreateOpts{
		Name: serverNameTemplate,
		ServerType: &hcloud.ServerType{
			Name: template.Type,
		},
		Image: &hcloud.Image{
			Name: "ubuntu-16.04",
		},
	}

	serverOptsTemplate.SSHKeys = append(serverOptsTemplate.SSHKeys, sshKey)

	for i := 1; i <= count; i++ {
		var serverOpts hcloud.ServerCreateOpts
		serverOpts = serverOptsTemplate
		serverOpts.Name = strings.Replace(serverNameTemplate, "@idx", fmt.Sprintf("%.02d", i), 1)

		// create
		server, err := cluster.runCreateServer(&serverOpts)

		if err != nil {
			return err
		}

		ipAddress := server.Server.PublicNet.IPv4.IP.String()
		log.Printf("Created node '%s' with IP %s", server.Server.Name, ipAddress)
		cluster.Nodes = append(cluster.Nodes, Node{
			Name:       serverOpts.Name,
			Type:       serverOpts.ServerType.Name,
			IsMaster:   template.IsMaster,
			IPAddress:  ipAddress,
			SSHKeyName: template.SSHKeyName,
		})
	}

	return nil
}

func (cluster *Cluster) ProvisionNodes() error {
	var wg sync.WaitGroup
	for _, node := range cluster.Nodes {
		// log.Printf("installing docker.io and kubeadm on node '%s'...", node.Name)
		wg.Add(1)
		go cluster.ProvisionNode(wg, node)
	}

	wg.Wait()

	return nil
}

func (cluster *Cluster) ProvisionNode(wg sync.WaitGroup, node Node) {
	cluster.coordinator.AddEvent(node.Name, "install packages")
	_, err := runCmd(node, "wget -cO- https://raw.githubusercontent.com/xetys/hetzner-kube/master/install-docker-kubeadm.sh | bash -")

	if err != nil {
		log.Fatalln(err)
	}

	if node.IsMaster {
		cluster.coordinator.AddEvent(node.Name, "packages installed")
	} else {
		cluster.coordinator.AddEvent(node.Name, "waiting for master")
	}

	wg.Done()
}

func (cluster *Cluster) runCreateServer(opts *hcloud.ServerCreateOpts) (*hcloud.ServerCreateResult, error) {

	log.Printf("creating server '%s'...", opts.Name)
	result, _, err := AppConf.Client.Server.Create(AppConf.Context, *opts)
	if err != nil {
		if err.(hcloud.Error).Code == "uniqueness_error" {
			server, _, err := AppConf.Client.Server.Get(AppConf.Context, opts.Name)

			if err != nil {
				return nil, err
			}

			log.Printf("loading server '%s'...", opts.Name)
			return &hcloud.ServerCreateResult{Server: server}, nil
		}

		return nil, err
	}

	if err := AppConf.ActionProgress(AppConf.Context, result.Action); err != nil {
		return nil, err
	}

	cluster.wait = true

	return &result, nil
}

func (cluster *Cluster) CreateMasterNodes(sshKeyName string, masterServerType string, count int) error {
	template := Node{SSHKeyName: sshKeyName, IsMaster: true, Type: masterServerType}
	log.Println("creating master nodes...")
	err := cluster.CreateNodes("master", template, count)
	saveCluster(cluster)
	return err
}

func (cluster *Cluster) CreateWorkerNodes(sshKeyName string, workerServerType string, count int) error {
	template := Node{SSHKeyName: sshKeyName, IsMaster: false, Type: workerServerType}
	err := cluster.CreateNodes("worker", template, count)
	saveCluster(cluster)
	return err
}

func (cluster *Cluster) InstallMaster() error {
	commands := []SSHCommand{
		{"disable swap", "swapoff -a"},
		{"kubeadm init", "kubeadm reset && kubeadm init --pod-network-cidr=10.244.0.0/16"},
		{"configure kubectl", "mkdir -p $HOME/.kube && cp -i /etc/kubernetes/admin.conf $HOME/.kube/config && chown $(id -u):$(id -g) $HOME/.kube/config"},
		{"install flannel", "kubectl apply -f https://raw.githubusercontent.com/coreos/flannel/v0.9.1/Documentation/kube-flannel.yml"},
		{"configure flannel", "kubectl -n kube-system patch ds kube-flannel-ds --type json -p '[{\"op\":\"add\",\"path\":\"/spec/template/spec/tolerations/-\",\"value\":{\"key\":\"node.cloudprovider.kubernetes.io/uninitialized\",\"value\":\"true\",\"effect\":\"NoSchedule\"}}]'"},
		{"install hcloud integration", fmt.Sprintf("kubectl -n kube-system create secret generic hcloud --from-literal=token=%s", AppConf.CurrentContext.Token)},
		{"deploy cloud controller manager", "kubectl apply -f  https://raw.githubusercontent.com/hetznercloud/hcloud-cloud-controller-manager/master/deploy/v1.0.0.yaml"},
	}
	for _, node := range cluster.Nodes {
		if node.IsMaster {
			if len(cluster.Nodes) == 1 {
				commands = append(commands, SSHCommand{"taint master", "kubectl taint nodes --all node-role.kubernetes.io/master-"})
			}

			for _, command := range commands {
				cluster.coordinator.AddEvent(node.Name, command.eventName)
				_, err := runCmd(node, command.command)
				if err != nil {
					return err
				}
			}

			cluster.coordinator.AddEvent(node.Name, "complete!")
			break
		}
	}

	return nil
}