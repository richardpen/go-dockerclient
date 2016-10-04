// Copyright 2016 go-dockerclient authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package testing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/docker/engine-api/types/swarm"
	"github.com/fsouza/go-dockerclient"
	"github.com/gorilla/mux"
)

type swarmServer struct {
	srv      *DockerServer
	mux      *mux.Router
	listener net.Listener
}

func newSwarmServer(srv *DockerServer, bind string) (*swarmServer, error) {
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, err
	}
	router := mux.NewRouter()
	router.Path("/internal/updatenodes").Methods("POST").HandlerFunc(srv.handlerWrapper(srv.internalUpdateNodes))
	server := &swarmServer{
		listener: listener,
		mux:      router,
		srv:      srv,
	}
	go http.Serve(listener, router)
	return server, nil
}

func (s *swarmServer) URL() string {
	if s.listener == nil {
		return ""
	}
	return "http://" + s.listener.Addr().String() + "/"
}

func (s *DockerServer) swarmInit(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm != nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	var req swarm.InitRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil && err != io.EOF {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	node, err := s.initSwarmNode(req.ListenAddr, req.AdvertiseAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	node.ManagerStatus.Leader = true
	err = s.runNodeOperation(s.swarmServer.URL(), nodeOperation{
		Op:   "add",
		Node: node,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.swarm = &swarm.Swarm{
		JoinTokens: swarm.JoinTokens{
			Manager: s.generateID(),
			Worker:  s.generateID(),
		},
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(s.nodeID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *DockerServer) swarmInspect(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
	} else {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.swarm)
	}
}

func (s *DockerServer) swarmJoin(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm != nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	var req swarm.JoinRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(req.RemoteAddrs) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	node, err := s.initSwarmNode(req.ListenAddr, req.AdvertiseAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = s.runNodeOperation(fmt.Sprintf("http://%s", req.RemoteAddrs[0]), nodeOperation{
		Op:   "add",
		Node: node,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.swarm = &swarm.Swarm{
		JoinTokens: swarm.JoinTokens{
			Manager: s.generateID(),
			Worker:  s.generateID(),
		},
	}
	w.WriteHeader(http.StatusOK)
}

func (s *DockerServer) swarmLeave(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
	} else {
		s.swarmServer.listener.Close()
		s.swarm = nil
		s.nodes = nil
		s.swarmServer = nil
		s.nodeID = ""
		w.WriteHeader(http.StatusOK)
	}
}

func (s *DockerServer) containerForService(srv *swarm.Service, name string) *docker.Container {
	portBindings := map[docker.Port][]docker.PortBinding{}
	exposedPort := map[docker.Port]struct{}{}
	if srv.Spec.EndpointSpec != nil {
		for _, p := range srv.Spec.EndpointSpec.Ports {
			targetPort := fmt.Sprintf("%d/%s", p.TargetPort, p.Protocol)
			portBindings[docker.Port(targetPort)] = []docker.PortBinding{
				{HostIP: "0.0.0.0", HostPort: fmt.Sprintf("%d", p.PublishedPort)},
			}
			exposedPort[docker.Port(targetPort)] = struct{}{}
		}
	}
	hostConfig := docker.HostConfig{
		PortBindings: portBindings,
	}
	dockerConfig := docker.Config{
		Entrypoint:   srv.Spec.TaskTemplate.ContainerSpec.Command,
		Cmd:          srv.Spec.TaskTemplate.ContainerSpec.Args,
		Env:          srv.Spec.TaskTemplate.ContainerSpec.Env,
		ExposedPorts: exposedPort,
	}
	return &docker.Container{
		ID:         s.generateID(),
		Name:       name,
		Image:      srv.Spec.TaskTemplate.ContainerSpec.Image,
		Created:    time.Now(),
		Config:     &dockerConfig,
		HostConfig: &hostConfig,
		State: docker.State{
			Running:   true,
			StartedAt: time.Now(),
			Pid:       rand.Int() % 50000,
			ExitCode:  0,
		},
	}
}

func (s *DockerServer) serviceCreate(w http.ResponseWriter, r *http.Request) {
	var config swarm.ServiceSpec
	defer r.Body.Close()
	err := json.NewDecoder(r.Body).Decode(&config)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.cMut.Lock()
	defer s.cMut.Unlock()
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if len(s.nodes) == 0 || s.swarm == nil {
		http.Error(w, "no swarm nodes available", http.StatusNotAcceptable)
		return
	}
	if config.Name == "" {
		config.Name = s.generateID()
	}
	for _, s := range s.services {
		if s.Spec.Name == config.Name {
			http.Error(w, "there's already a service with this name", http.StatusConflict)
			return
		}
	}
	service := swarm.Service{
		ID:   s.generateID(),
		Spec: config,
	}
	containerCount := 1
	if service.Spec.Mode.Global != nil {
		containerCount = len(s.nodes)
	} else if repl := service.Spec.Mode.Replicated; repl != nil {
		if repl.Replicas != nil {
			containerCount = int(*repl.Replicas)
		}
	}
	for i := 0; i < containerCount; i++ {
		container := s.containerForService(&service, fmt.Sprintf("%s-%d", config.Name, i))
		chosenNode := s.nodes[s.nodeRR]
		s.nodeRR = (s.nodeRR + 1) % len(s.nodes)
		task := swarm.Task{
			ID:        s.generateID(),
			ServiceID: service.ID,
			NodeID:    chosenNode.ID,
			Status: swarm.TaskStatus{
				State: swarm.TaskStateReady,
				ContainerStatus: swarm.ContainerStatus{
					ContainerID: container.ID,
				},
			},
			DesiredState: swarm.TaskStateReady,
			Spec:         config.TaskTemplate,
		}
		s.tasks = append(s.tasks, &task)
		s.containers = append(s.containers, container)
		s.notify(container)
	}
	s.services = append(s.services, &service)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(service)
}

func (s *DockerServer) serviceInspect(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	id := mux.Vars(r)["id"]
	for _, srv := range s.services {
		if srv.ID == id || srv.Spec.Name == id {
			json.NewEncoder(w).Encode(srv)
			return
		}
	}
	http.Error(w, "service not found", http.StatusNotFound)
}

func (s *DockerServer) taskInspect(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	id := mux.Vars(r)["id"]
	for _, task := range s.tasks {
		if task.ID == id {
			json.NewEncoder(w).Encode(task)
			return
		}
	}
	http.Error(w, "task not found", http.StatusNotFound)
}

func (s *DockerServer) serviceList(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	filtersRaw := r.FormValue("filters")
	var filters map[string][]string
	json.Unmarshal([]byte(filtersRaw), &filters)
	if filters == nil {
		json.NewEncoder(w).Encode(s.services)
		return
	}
	var ret []*swarm.Service
	for i, srv := range s.services {
		if inFilter(filters["id"], srv.ID) ||
			inFilter(filters["name"], srv.Spec.Name) {
			ret = append(ret, s.services[i])
		}
	}
	json.NewEncoder(w).Encode(ret)
}

func (s *DockerServer) taskList(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	filtersRaw := r.FormValue("filters")
	var filters map[string][]string
	json.Unmarshal([]byte(filtersRaw), &filters)
	if filters == nil {
		json.NewEncoder(w).Encode(s.tasks)
		return
	}
	var ret []*swarm.Task
	for i, task := range s.tasks {
		var srv *swarm.Service
		for _, srv = range s.services {
			if task.ServiceID == srv.ID {
				break
			}
		}
		if srv == nil {
			http.Error(w, "service not found", http.StatusNotFound)
			return
		}
		if inFilter(filters["id"], task.ID) ||
			inFilter(filters["service"], task.ServiceID) ||
			inFilter(filters["service"], srv.Spec.Name) ||
			inFilter(filters["node"], task.NodeID) ||
			inFilter(filters["desired-state"], string(task.DesiredState)) ||
			inLabelFilter(filters["label"], srv.Spec.Annotations.Labels) {
			ret = append(ret, s.tasks[i])
		}
	}
	json.NewEncoder(w).Encode(ret)
}

func inLabelFilter(list []string, labels map[string]string) bool {
	for _, item := range list {
		parts := strings.Split(item, "=")
		key := parts[0]
		if val, ok := labels[key]; ok {
			if len(parts) > 1 && val != parts[1] {
				continue
			}
			return true
		}
	}
	return false
}

func inFilter(list []string, wanted string) bool {
	for _, item := range list {
		if item == wanted {
			return true
		}
	}
	return false
}

func (s *DockerServer) serviceDelete(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	s.cMut.Lock()
	defer s.cMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	id := mux.Vars(r)["id"]
	var i int
	var toDelete *swarm.Service
	for i = range s.services {
		if s.services[i].ID == id || s.services[i].Spec.Name == id {
			toDelete = s.services[i]
			break
		}
	}
	if toDelete == nil {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	s.services[i] = s.services[len(s.services)-1]
	s.services = s.services[:len(s.services)-1]
	for i := 0; i < len(s.tasks); i++ {
		if s.tasks[i].ServiceID == toDelete.ID {
			_, contIdx, _ := s.findContainerWithLock(s.tasks[i].Status.ContainerStatus.ContainerID, false)
			if contIdx != -1 {
				s.containers = append(s.containers[:contIdx], s.containers[contIdx+1:]...)
			}
			s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)
			i--
		}
	}
}

func (s *DockerServer) serviceUpdate(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	s.cMut.Lock()
	defer s.cMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	id := mux.Vars(r)["id"]
	var toUpdate *swarm.Service
	for i := range s.services {
		if s.services[i].ID == id || s.services[i].Spec.Name == id {
			toUpdate = s.services[i]
			break
		}
	}
	if toUpdate == nil {
		http.Error(w, "service not found", http.StatusNotFound)
		return
	}
	var newSpec swarm.ServiceSpec
	json.NewDecoder(r.Body).Decode(&newSpec)
	toUpdate.Spec = newSpec
	var newTasks []*swarm.Task
	var newContainers []*docker.Container
	for i := 0; i < len(s.tasks); i++ {
		if s.tasks[i].ServiceID != toUpdate.ID {
			continue
		}
		_, contIdx, _ := s.findContainerWithLock(s.tasks[i].Status.ContainerStatus.ContainerID, false)
		if contIdx != -1 {
			s.containers = append(s.containers[:contIdx], s.containers[contIdx+1:]...)
		}
		container := s.containerForService(toUpdate, fmt.Sprintf("%s-%d-updated", toUpdate.Spec.Name, i))
		chosenNode := s.nodes[s.nodeRR]
		s.nodeRR = (s.nodeRR + 1) % len(s.nodes)
		task := swarm.Task{
			ID:        s.generateID(),
			ServiceID: toUpdate.ID,
			NodeID:    chosenNode.ID,
			Status: swarm.TaskStatus{
				State: swarm.TaskStateReady,
				ContainerStatus: swarm.ContainerStatus{
					ContainerID: container.ID,
				},
			},
			DesiredState: swarm.TaskStateReady,
			Spec:         toUpdate.Spec.TaskTemplate,
		}
		s.tasks = append(s.tasks[:i], s.tasks[i+1:]...)
		i--
		newTasks = append(newTasks, &task)
		newContainers = append(newContainers, container)
		s.notify(container)
	}
	s.containers = append(s.containers, newContainers...)
	s.tasks = append(s.tasks, newTasks...)
}

func (s *DockerServer) nodeUpdate(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	id := mux.Vars(r)["id"]
	var n *swarm.Node
	for i := range s.nodes {
		if s.nodes[i].ID == id {
			n = &s.nodes[i]
			break
		}
	}
	if n == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var spec swarm.NodeSpec
	err := json.NewDecoder(r.Body).Decode(&spec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n.Spec = spec
	err = s.runNodeOperation(s.swarmServer.URL(), nodeOperation{
		Op:   "update",
		Node: *n,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *DockerServer) nodeDelete(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	id := mux.Vars(r)["id"]
	err := s.runNodeOperation(s.swarmServer.URL(), nodeOperation{
		Op: "delete",
		Node: swarm.Node{
			ID: id,
		},
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *DockerServer) nodeInspect(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	id := mux.Vars(r)["id"]
	for _, n := range s.nodes {
		if n.ID == id {
			err := json.NewEncoder(w).Encode(n)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func (s *DockerServer) nodeList(w http.ResponseWriter, r *http.Request) {
	s.swarmMut.Lock()
	defer s.swarmMut.Unlock()
	if s.swarm == nil {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	err := json.NewEncoder(w).Encode(s.nodes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type nodeOperation struct {
	Op   string
	Node swarm.Node
}

func (s *DockerServer) runNodeOperation(dst string, nodeOp nodeOperation) error {
	data, err := json.Marshal(nodeOp)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/internal/updatenodes", strings.TrimRight(dst, "/"))
	rsp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	if rsp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code in updatenodes: %d", rsp.StatusCode)
	}
	return json.NewDecoder(rsp.Body).Decode(&s.nodes)
}

func (s *DockerServer) internalUpdateNodes(w http.ResponseWriter, r *http.Request) {
	propagate := r.URL.Query().Get("propagate") != "0"
	if !propagate {
		s.swarmMut.Lock()
		defer s.swarmMut.Unlock()
	}
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var nodeOp nodeOperation
	err = json.Unmarshal(data, &nodeOp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if propagate {
		for _, node := range s.nodes {
			if s.nodeID == node.ID {
				continue
			}
			url := fmt.Sprintf("http://%s/internal/updatenodes?propagate=0", node.ManagerStatus.Addr)
			_, err = http.Post(url, "application/json", bytes.NewReader(data))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}
	switch nodeOp.Op {
	case "add":
		s.nodes = append(s.nodes, nodeOp.Node)
	case "update":
		for i, n := range s.nodes {
			if n.ID == nodeOp.Node.ID {
				s.nodes[i] = nodeOp.Node
				break
			}
		}
	case "delete":
		for i, n := range s.nodes {
			if n.ID == nodeOp.Node.ID {
				s.nodes = append(s.nodes[:i], s.nodes[i+1:]...)
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	err = json.NewEncoder(w).Encode(s.nodes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
