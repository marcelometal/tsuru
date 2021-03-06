// Copyright 2014 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"bytes"
	"fmt"
	"github.com/fsouza/go-dockerclient"
	dtesting "github.com/fsouza/go-dockerclient/testing"
	"github.com/tsuru/config"
	"github.com/tsuru/docker-cluster/cluster"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/db"
	etesting "github.com/tsuru/tsuru/exec/testing"
	ftesting "github.com/tsuru/tsuru/fs/testing"
	"github.com/tsuru/tsuru/provision"
	rtesting "github.com/tsuru/tsuru/router/testing"
	"github.com/tsuru/tsuru/safe"
	"github.com/tsuru/tsuru/testing"
	"labix.org/v2/mgo/bson"
	"launchpad.net/gocheck"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func (s *S) TestContainerGetAddress(c *gocheck.C) {
	container := container{ID: "id123", HostAddr: "10.10.10.10", HostPort: "49153"}
	address := container.getAddress()
	expected := "http://10.10.10.10:49153"
	c.Assert(address, gocheck.Equals, expected)
}

func (s *S) TestContainerCreate(c *gocheck.C) {
	oldClusterNodes := clusterNodes
	clusterNodes = map[string]string{"server": s.server.URL()}
	defer func() { clusterNodes = oldClusterNodes }()
	app := testing.NewFakeApp("app-name", "brainfuck", 1)
	app.Memory = 15
	rtesting.FakeRouter.AddBackend(app.GetName())
	defer rtesting.FakeRouter.RemoveBackend(app.GetName())
	dockerCluster().PullImage(
		docker.PullImageOptions{Repository: "tsuru/brainfuck"},
		docker.AuthConfiguration{},
	)
	cont := container{Name: "myName", AppName: app.GetName(), Type: app.GetPlatform(), Status: "created"}
	err := cont.create(app, getImage(app), []string{"docker", "run"})
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(&cont)
	c.Assert(cont.ID, gocheck.Not(gocheck.Equals), "")
	c.Assert(cont, gocheck.FitsTypeOf, container{})
	c.Assert(cont.AppName, gocheck.Equals, app.GetName())
	c.Assert(cont.Type, gocheck.Equals, app.GetPlatform())
	u, _ := url.Parse(s.server.URL())
	host, _, _ := net.SplitHostPort(u.Host)
	c.Assert(cont.HostAddr, gocheck.Equals, host)
	user, err := config.GetString("docker:ssh:user")
	c.Assert(err, gocheck.IsNil)
	dcli, _ := docker.NewClient(s.server.URL())
	container, err := dcli.InspectContainer(cont.ID)
	c.Assert(err, gocheck.IsNil)
	c.Assert(container.Path, gocheck.Equals, "docker")
	c.Assert(container.Args, gocheck.DeepEquals, []string{"run"})
	c.Assert(container.Config.User, gocheck.Equals, user)
	c.Assert(container.Config.Memory, gocheck.Equals, float64(app.Memory*1024*1024))
}

func (s *S) TestContainerCreateUndefinedUser(c *gocheck.C) {
	oldUser, _ := config.Get("docker:ssh:user")
	defer config.Set("docker:ssh:user", oldUser)
	config.Unset("docker:ssh:user")
	oldClusterNodes := clusterNodes
	clusterNodes = map[string]string{"server": s.server.URL()}
	defer func() { clusterNodes = oldClusterNodes }()
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	app := testing.NewFakeApp("app-name", "python", 1)
	rtesting.FakeRouter.AddBackend(app.GetName())
	defer rtesting.FakeRouter.RemoveBackend(app.GetName())
	cont := container{Name: "myName", AppName: app.GetName(), Type: app.GetPlatform(), Status: "created"}
	err = cont.create(app, getImage(app), []string{"docker", "run"})
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(&cont)
	dcli, _ := docker.NewClient(s.server.URL())
	container, err := dcli.InspectContainer(cont.ID)
	c.Assert(err, gocheck.IsNil)
	c.Assert(container.Config.User, gocheck.Equals, "")
}

func (s *S) TestGetSSHCommandsDefaultSSHDPath(c *gocheck.C) {
	rfs := ftesting.RecordingFs{}
	f, err := rfs.Create("/opt/me/id_dsa.pub")
	c.Assert(err, gocheck.IsNil)
	f.Write([]byte("ssh-rsa ohwait! me@machine"))
	f.Close()
	old := fsystem
	fsystem = &rfs
	defer func() {
		fsystem = old
	}()
	config.Set("docker:ssh:public-key", "/opt/me/id_dsa.pub")
	defer config.Unset("docker:ssh:public-key")
	commands, err := sshCmds()
	c.Assert(err, gocheck.IsNil)
	c.Assert(commands[1], gocheck.Equals, "sudo /usr/sbin/sshd -D")
}

func (s *S) TestGetSSHCommandsDefaultKeyFile(c *gocheck.C) {
	rfs := ftesting.RecordingFs{}
	f, err := rfs.Create(os.ExpandEnv("${HOME}/.ssh/id_rsa.pub"))
	c.Assert(err, gocheck.IsNil)
	f.Write([]byte("ssh-rsa ohwait! me@machine"))
	f.Close()
	old := fsystem
	fsystem = &rfs
	defer func() {
		fsystem = old
	}()
	commands, err := sshCmds()
	c.Assert(err, gocheck.IsNil)
	c.Assert(commands[0], gocheck.Equals, "/var/lib/tsuru/add-key ssh-rsa ohwait! me@machine")
}

func (s *S) TestGetSSHCommandsMissingAddKeyCommand(c *gocheck.C) {
	old, _ := config.Get("docker:ssh:add-key-cmd")
	defer config.Set("docker:ssh:add-key-cmd", old)
	config.Unset("docker:ssh:add-key-cmd")
	commands, err := sshCmds()
	c.Assert(commands, gocheck.IsNil)
	c.Assert(err, gocheck.NotNil)
}

func (s *S) TestGetSSHCommandsKeyFileNotFound(c *gocheck.C) {
	old := fsystem
	fsystem = &ftesting.RecordingFs{}
	defer func() {
		fsystem = old
	}()
	commands, err := sshCmds()
	c.Assert(commands, gocheck.IsNil)
	c.Assert(err, gocheck.NotNil)
	c.Assert(os.IsNotExist(err), gocheck.Equals, true)
}

func (s *S) TestGetPort(c *gocheck.C) {
	port, err := getPort()
	c.Assert(err, gocheck.IsNil)
	c.Assert(port, gocheck.Equals, s.port)
}

func (s *S) TestGetPortUndefined(c *gocheck.C) {
	old, _ := config.Get("docker:run-cmd:port")
	defer config.Set("docker:run-cmd:port", old)
	config.Unset("docker:run-cmd:port")
	port, err := getPort()
	c.Assert(port, gocheck.Equals, "")
	c.Assert(err, gocheck.NotNil)
}

func (s *S) TestGetPortInteger(c *gocheck.C) {
	old, _ := config.Get("docker:run-cmd:port")
	defer config.Set("docker:run-cmd:port", old)
	config.Set("docker:run-cmd:port", 8888)
	port, err := getPort()
	c.Assert(err, gocheck.IsNil)
	c.Assert(port, gocheck.Equals, "8888")
}

func (s *S) TestContainerSetStatus(c *gocheck.C) {
	container := container{ID: "something-300"}
	coll := collection()
	defer coll.Close()
	coll.Insert(container)
	defer coll.Remove(bson.M{"id": container.ID})
	container.setStatus("what?!")
	c2, err := getContainer(container.ID)
	c.Assert(err, gocheck.IsNil)
	c.Assert(c2.Status, gocheck.Equals, "what?!")
}

func (s *S) TestContainerSetImage(c *gocheck.C) {
	container := container{ID: "something-300"}
	coll := collection()
	defer coll.Close()
	coll.Insert(container)
	defer coll.Remove(bson.M{"id": container.ID})
	container.setImage("newimage")
	c2, err := getContainer(container.ID)
	c.Assert(err, gocheck.IsNil)
	c.Assert(c2.Image, gocheck.Equals, "newimage")
}

func newImage(repo, serverURL string) error {
	var buf safe.Buffer
	opts := docker.PullImageOptions{Repository: repo, OutputStream: &buf}
	return dCluster.PullImage(opts, docker.AuthConfiguration{})
}

type newContainerOpts struct {
	AppName string
}

func (s *S) newContainer(opts *newContainerOpts) (*container, error) {
	appName := "container"
	if opts != nil {
		appName = opts.AppName
	}
	container := container{
		AppName:  appName,
		ID:       "id",
		IP:       "10.10.10.10",
		HostPort: "3333",
		HostAddr: "127.0.0.1",
	}
	rtesting.FakeRouter.AddBackend(container.AppName)
	rtesting.FakeRouter.AddRoute(container.AppName, container.getAddress())
	port, err := getPort()
	if err != nil {
		return nil, err
	}
	ports := make(map[docker.Port]struct{})
	ports[docker.Port(fmt.Sprintf("%s/tcp", port))] = struct{}{}
	config := docker.Config{
		Image:        "tsuru/python",
		Cmd:          []string{"ps"},
		ExposedPorts: ports,
	}
	_, c, err := dCluster.CreateContainer(docker.CreateContainerOptions{Config: &config})
	if err != nil {
		return nil, err
	}
	container.ID = c.ID
	container.Image = "tsuru/python"
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	err = conn.Collection(s.collName).Insert(&container)
	if err != nil {
		return nil, err
	}
	return &container, err
}

func (s *S) removeTestContainer(c *container) error {
	rtesting.FakeRouter.RemoveBackend(c.AppName)
	return c.remove()
}

func (s *S) TestContainerRemove(c *gocheck.C) {
	handler, cleanup := startSSHAgentServer("")
	defer cleanup()
	fexec := &etesting.FakeExecutor{}
	setExecut(fexec)
	defer setExecut(nil)
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	container, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(container)
	err = container.remove()
	c.Assert(err, gocheck.IsNil)
	c.Assert(handler.requests[0].Method, gocheck.Equals, "DELETE")
	c.Assert(handler.requests[0].URL.Path, gocheck.Equals, "/container/"+container.IP)
	coll := collection()
	defer coll.Close()
	err = coll.Find(bson.M{"id": container.ID}).One(&container)
	c.Assert(err, gocheck.NotNil)
	c.Assert(err.Error(), gocheck.Equals, "not found")
	c.Assert(rtesting.FakeRouter.HasRoute(container.AppName, container.getAddress()), gocheck.Equals, false)
	client, _ := docker.NewClient(s.server.URL())
	_, err = client.InspectContainer(container.ID)
	c.Assert(err, gocheck.NotNil)
	_, ok := err.(*docker.NoSuchContainer)
	c.Assert(ok, gocheck.Equals, true)
}

func (s *S) TestRemoveContainerIgnoreErrors(c *gocheck.C) {
	handler, cleanup := startSSHAgentServer("")
	defer cleanup()
	fexec := &etesting.FakeExecutor{}
	setExecut(fexec)
	defer setExecut(nil)
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	container, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(container)
	client, _ := docker.NewClient(s.server.URL())
	err = client.RemoveContainer(docker.RemoveContainerOptions{ID: container.ID})
	c.Assert(err, gocheck.IsNil)
	err = container.remove()
	c.Assert(err, gocheck.IsNil)
	c.Assert(handler.requests[0].Method, gocheck.Equals, "DELETE")
	c.Assert(handler.requests[0].URL.Path, gocheck.Equals, "/container/"+container.IP)
	coll := collection()
	defer coll.Close()
	err = coll.Find(bson.M{"id": container.ID}).One(&container)
	c.Assert(err, gocheck.NotNil)
	c.Assert(err.Error(), gocheck.Equals, "not found")
	c.Assert(rtesting.FakeRouter.HasRoute(container.AppName, container.getAddress()), gocheck.Equals, false)
}

func (s *S) TestContainerRemoveHost(c *gocheck.C) {
	var handler FakeSSHServer
	handler.output = ". .."
	server := httptest.NewServer(&handler)
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	config.Set("docker:ssh-agent-port", portNumber)
	defer config.Unset("docker:ssh-agent-port")
	container := container{ID: "c-036", AppName: "starbreaker", Type: "python", IP: "10.10.10.1", HostAddr: host}
	err := container.removeHost()
	c.Assert(err, gocheck.IsNil)
	request := handler.requests[0]
	c.Assert(request.Method, gocheck.Equals, "DELETE")
	c.Assert(request.URL.Path, gocheck.Equals, "/container/10.10.10.1")
}

func (s *S) TestContainerNetworkInfo(c *gocheck.C) {
	_, cleanup := startSSHAgentServer("")
	defer cleanup()
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	cont, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(cont)
	ip, port, err := cont.networkInfo()
	c.Assert(err, gocheck.IsNil)
	c.Assert(ip, gocheck.Not(gocheck.Equals), "")
	c.Assert(port, gocheck.Not(gocheck.Equals), "")
}

func (s *S) TestContainerNetworkInfoNotFound(c *gocheck.C) {
	inspectOut := `{
	"NetworkSettings": {
		"IpAddress": "10.10.10.10",
		"IpPrefixLen": 8,
		"Gateway": "10.65.41.1",
		"Ports": {}
	}
}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/containers/") {
			w.Write([]byte(inspectOut))
		}
	}))
	defer server.Close()
	var storage mapStorage
	storage.StoreContainer("c-01", "server")
	oldCluster := dockerCluster()
	var err error
	dCluster, err = cluster.New(nil, &storage,
		cluster.Node{ID: "server", Address: server.URL},
	)
	c.Assert(err, gocheck.IsNil)
	defer func() {
		dCluster = oldCluster
	}()
	container := container{ID: "c-01"}
	ip, port, err := container.networkInfo()
	c.Assert(ip, gocheck.Equals, "")
	c.Assert(port, gocheck.Equals, "")
	c.Assert(err, gocheck.NotNil)
	c.Assert(err.Error(), gocheck.Equals, "Container port 8888 is not mapped to any host port")
}

func (s *S) TestContainerSSH(c *gocheck.C) {
	var handler FakeSSHServer
	handler.output = ". .."
	server := httptest.NewServer(&handler)
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	config.Set("docker:ssh-agent-port", portNumber)
	defer config.Unset("docker:ssh-agent-port")
	var stdout, stderr bytes.Buffer
	container, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(container)
	container.HostAddr = host
	err = container.ssh(&stdout, &stderr, "ls", "-a")
	c.Assert(err, gocheck.IsNil)
	c.Assert(stdout.String(), gocheck.Equals, handler.output)
	body := handler.bodies[0]
	input := cmdInput{Cmd: "ls", Args: []string{"-a"}}
	c.Assert(body, gocheck.DeepEquals, input)
}

func (s *S) TestContainerSSHFiltersStdout(c *gocheck.C) {
	var handler FakeSSHServer
	handler.output = "failed\nunable to resolve host abcdef"
	server := httptest.NewServer(&handler)
	defer server.Close()
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	portNumber, _ := strconv.Atoi(port)
	config.Set("docker:ssh-agent-port", portNumber)
	defer config.Unset("docker:ssh-agent-port")
	var stdout, stderr bytes.Buffer
	container, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(container)
	container.HostAddr = host
	err = container.ssh(&stdout, &stderr, "ls", "-a")
	c.Assert(err, gocheck.IsNil)
	c.Assert(stdout.String(), gocheck.Equals, "failed\n")
}

func (s *S) TestGetContainer(c *gocheck.C) {
	coll := collection()
	defer coll.Close()
	coll.Insert(
		container{ID: "abcdef", Type: "python"},
		container{ID: "fedajs", Type: "ruby"},
		container{ID: "wat", Type: "java"},
	)
	defer coll.RemoveAll(bson.M{"id": bson.M{"$in": []string{"abcdef", "fedajs", "wat"}}})
	container, err := getContainer("abcdef")
	c.Assert(err, gocheck.IsNil)
	c.Assert(container.ID, gocheck.Equals, "abcdef")
	c.Assert(container.Type, gocheck.Equals, "python")
	container, err = getContainer("wut")
	c.Assert(container, gocheck.IsNil)
	c.Assert(err.Error(), gocheck.Equals, "not found")
}

func (s *S) TestGetContainers(c *gocheck.C) {
	coll := collection()
	defer coll.Close()
	coll.Insert(
		container{ID: "abcdef", Type: "python", AppName: "something"},
		container{ID: "fedajs", Type: "python", AppName: "something"},
		container{ID: "wat", Type: "java", AppName: "otherthing"},
	)
	defer coll.RemoveAll(bson.M{"id": bson.M{"$in": []string{"abcdef", "fedajs", "wat"}}})
	containers, err := listContainersByApp("something")
	c.Assert(err, gocheck.IsNil)
	c.Assert(containers, gocheck.HasLen, 2)
	c.Assert(containers[0].ID, gocheck.Equals, "abcdef")
	c.Assert(containers[1].ID, gocheck.Equals, "fedajs")
	containers, err = listContainersByApp("otherthing")
	c.Assert(err, gocheck.IsNil)
	c.Assert(containers, gocheck.HasLen, 1)
	c.Assert(containers[0].ID, gocheck.Equals, "wat")
	containers, err = listContainersByApp("unknown")
	c.Assert(err, gocheck.IsNil)
	c.Assert(containers, gocheck.HasLen, 0)
}

func (s *S) TestGetImageFromAppPlatform(c *gocheck.C) {
	app := testing.NewFakeApp("myapp", "python", 1)
	img := getImage(app)
	repoNamespace, err := config.GetString("docker:repository-namespace")
	c.Assert(err, gocheck.IsNil)
	c.Assert(img, gocheck.Equals, fmt.Sprintf("%s/python", repoNamespace))
}

func (s *S) TestGetImageAppWhenDeployIsMultipleOf10(c *gocheck.C) {
	var request http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = *r
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	imageRepo := u.Host + "/tsuru/python"
	err := newImage(imageRepo, s.server.URL())
	c.Assert(err, gocheck.IsNil)
	conn, err := db.Conn()
	c.Assert(err, gocheck.IsNil)
	defer conn.Close()
	app := &app.App{Name: "app1", Platform: "python", Deploys: 20}
	err = conn.Apps().Insert(app)
	c.Assert(err, gocheck.IsNil)
	defer conn.Apps().Remove(bson.M{"name": "app1"})
	cont := container{ID: "bleble", Type: "python", AppName: "app1", Image: imageRepo}
	coll := collection()
	err = coll.Insert(cont)
	c.Assert(err, gocheck.IsNil)
	defer coll.Close()
	c.Assert(err, gocheck.IsNil)
	defer coll.RemoveAll(bson.M{"id": "bleble"})
	img := getImage(app)
	repoNamespace, err := config.GetString("docker:repository-namespace")
	c.Assert(err, gocheck.IsNil)
	c.Assert(img, gocheck.Equals, fmt.Sprintf("%s/python", repoNamespace))
	c.Assert(request.Method, gocheck.Equals, "DELETE")
	path := "/v1/repositories/tsuru/python/tags"
	c.Assert(request.URL.Path, gocheck.Equals, path)
}

func (s *S) TestGetImageFromDatabase(c *gocheck.C) {
	cont := container{ID: "bleble", Type: "python", AppName: "myapp", Image: "someimageid"}
	coll := collection()
	err := coll.Insert(cont)
	defer coll.Close()
	c.Assert(err, gocheck.IsNil)
	defer coll.RemoveAll(bson.M{"id": "bleble"})
	app := testing.NewFakeApp("myapp", "python", 1)
	img := getImage(app)
	c.Assert(img, gocheck.Equals, "someimageid")
}

func (s *S) TestGetImageWithRegistry(c *gocheck.C) {
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	app := testing.NewFakeApp("myapp", "python", 1)
	img := getImage(app)
	repoNamespace, _ := config.GetString("docker:repository-namespace")
	expected := fmt.Sprintf("localhost:3030/%s/python", repoNamespace)
	c.Assert(img, gocheck.Equals, expected)
}

func (s *S) TestContainerCommit(c *gocheck.C) {
	_, cleanup := startSSHAgentServer("")
	defer cleanup()
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	cont, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(cont)
	imageId, err := cont.commit()
	c.Assert(err, gocheck.IsNil)
	repoNamespace, _ := config.GetString("docker:repository-namespace")
	repository := repoNamespace + "/" + cont.AppName
	c.Assert(imageId, gocheck.Equals, repository)
}

func (s *S) TestContainerCommitWithRegistry(c *gocheck.C) {
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	_, cleanup := startSSHAgentServer("")
	defer cleanup()
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	cont, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(cont)
	imageId, err := cont.commit()
	c.Assert(err, gocheck.IsNil)
	repoNamespace, _ := config.GetString("docker:repository-namespace")
	repository := "localhost:3030/" + repoNamespace + "/" + cont.AppName
	c.Assert(imageId, gocheck.Equals, repository)
}

func (s *S) TestRemoveImage(c *gocheck.C) {
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	client, err := docker.NewClient(s.server.URL())
	c.Assert(err, gocheck.IsNil)
	images, err := client.ListImages(true)
	c.Assert(err, gocheck.IsNil)
	err = removeImage(images[0].ID)
	c.Assert(err, gocheck.IsNil)
}

func (s *S) TestRemoveImageCallsRegistry(c *gocheck.C) {
	var request http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request = *r
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	imageRepo := u.Host + "/tsuru/python"
	err := newImage(imageRepo, s.server.URL())
	c.Assert(err, gocheck.IsNil)
	err = removeImage(imageRepo)
	c.Assert(err, gocheck.IsNil)
	c.Assert(request.Method, gocheck.Equals, "DELETE")
	path := "/v1/repositories/tsuru/python/tags"
	c.Assert(request.URL.Path, gocheck.Equals, path)
}

func (s *S) TestGitDeploy(c *gocheck.C) {
	h := &testing.TestHandler{}
	gandalfServer := testing.StartGandalfTestServer(h)
	defer gandalfServer.Close()
	go s.stopContainers(1)
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	app := testing.NewFakeApp("myapp", "python", 1)
	rtesting.FakeRouter.AddBackend(app.GetName())
	defer rtesting.FakeRouter.RemoveBackend(app.GetName())
	var buf bytes.Buffer
	imageId, err := gitDeploy(app, "ff13e", &buf)
	c.Assert(err, gocheck.IsNil)
	c.Assert(imageId, gocheck.Equals, "tsuru/myapp")
	var conts []container
	coll := collection()
	defer coll.Close()
	err = coll.Find(nil).All(&conts)
	c.Assert(err, gocheck.IsNil)
	c.Assert(conts, gocheck.HasLen, 0)
	err = dockerCluster().RemoveImage("tsuru/myapp")
	c.Assert(err, gocheck.IsNil)
}

type errBuffer struct{}

func (errBuffer) Write(data []byte) (int, error) {
	return 0, fmt.Errorf("My write error")
}

func (s *S) TestGitDeployRollsbackAfterErrorOnAttach(c *gocheck.C) {
	h := &testing.TestHandler{}
	gandalfServer := testing.StartGandalfTestServer(h)
	defer gandalfServer.Close()
	go s.stopContainers(1)
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	app := testing.NewFakeApp("myapp", "python", 1)
	rtesting.FakeRouter.AddBackend(app.GetName())
	defer rtesting.FakeRouter.RemoveBackend(app.GetName())
	var buf errBuffer
	_, err = gitDeploy(app, "ff13e", &buf)
	c.Assert(err, gocheck.NotNil)
	c.Assert(err.Error(), gocheck.Equals, "My write error")
	var conts []container
	coll := collection()
	defer coll.Close()
	err = coll.Find(nil).All(&conts)
	c.Assert(err, gocheck.IsNil)
	c.Assert(conts, gocheck.HasLen, 0)
	err = dockerCluster().RemoveImage("tsuru/myapp")
	c.Assert(err, gocheck.NotNil)
}

func (s *S) TestArchiveDeploy(c *gocheck.C) {
	go s.stopContainers(1)
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	app := testing.NewFakeApp("myapp", "python", 1)
	rtesting.FakeRouter.AddBackend(app.GetName())
	defer rtesting.FakeRouter.RemoveBackend(app.GetName())
	var buf bytes.Buffer
	_, err = archiveDeploy(app, "https://s3.amazonaws.com/wat/archive.tar.gz", &buf)
	c.Assert(err, gocheck.IsNil)
}

func (s *S) TestStart(c *gocheck.C) {
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	app := testing.NewFakeApp("myapp", "python", 1)
	imageId := getImage(app)
	rtesting.FakeRouter.AddBackend(app.GetName())
	defer rtesting.FakeRouter.RemoveBackend(app.GetName())
	var buf bytes.Buffer
	cont, err := start(app, imageId, &buf)
	c.Assert(err, gocheck.IsNil)
	defer cont.remove()
	c.Assert(cont.ID, gocheck.Not(gocheck.Equals), "")
	cont2, err := getContainer(cont.ID)
	c.Assert(err, gocheck.IsNil)
	c.Assert(cont2.Image, gocheck.Equals, imageId)
	c.Assert(cont2.Status, gocheck.Equals, provision.StatusStarted.String())
}

func (s *S) TestContainerRunCmdError(c *gocheck.C) {
	fexec := etesting.ErrorExecutor{
		FakeExecutor: etesting.FakeExecutor{
			Output: map[string][][]byte{"*": {[]byte("f1 f2 f3")}},
		},
	}
	setExecut(&fexec)
	defer setExecut(nil)
	_, err := runCmd("ls", "-a")
	e, ok := err.(*cmdError)
	c.Assert(ok, gocheck.Equals, true)
	c.Assert(e.err, gocheck.NotNil)
	c.Assert(e.out, gocheck.Equals, "f1 f2 f3")
	c.Assert(e.cmd, gocheck.Equals, "ls")
	c.Assert(e.args, gocheck.DeepEquals, []string{"-a"})
}

func (s *S) TestContainerStop(c *gocheck.C) {
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	cont, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(cont)
	client, err := docker.NewClient(s.server.URL())
	c.Assert(err, gocheck.IsNil)
	err = client.StartContainer(cont.ID, nil)
	c.Assert(err, gocheck.IsNil)
	err = cont.stop()
	c.Assert(err, gocheck.IsNil)
	dockerContainer, err := dockerCluster().InspectContainer(cont.ID)
	c.Assert(err, gocheck.IsNil)
	c.Assert(dockerContainer.State.Running, gocheck.Equals, false)
	c.Assert(cont.Status, gocheck.Equals, provision.StatusStopped.String())
}

func (s *S) TestContainerStopReturnsNilWhenContainerAlreadyMarkedAsStopped(c *gocheck.C) {
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	cont, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(cont)

	cont.setStatus(provision.StatusStopped.String())
	err = cont.stop()
	c.Assert(err, gocheck.IsNil)
}

func (s *S) TestContainerLogs(c *gocheck.C) {
	_, cleanup := startSSHAgentServer("")
	defer cleanup()
	err := newImage("tsuru/python", s.server.URL())
	c.Assert(err, gocheck.IsNil)
	cont, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(cont)
	var buff bytes.Buffer
	err = cont.logs(&buff)
	c.Assert(err, gocheck.IsNil)
	c.Assert(buff.String(), gocheck.Not(gocheck.Equals), "")
}

func (s *S) TestGetHostAddr(c *gocheck.C) {
	cmutex.Lock()
	old := dCluster
	var err error
	dCluster, err = cluster.New(nil, &mapStorage{},
		cluster.Node{ID: "server0", Address: "http://localhost:8081"},
		cluster.Node{ID: "server20", Address: "http://localhost:3234"},
		cluster.Node{ID: "server21", Address: "http://10.10.10.10:4243"},
	)
	c.Assert(err, gocheck.IsNil)
	cmutex.Unlock()
	defer func() {
		cmutex.Lock()
		dCluster = old
		cmutex.Unlock()
	}()
	var tests = []struct {
		input    string
		expected string
	}{
		{"server0", "localhost"},
		{"server20", "localhost"},
		{"server21", "10.10.10.10"},
		{"server33", ""},
	}
	for _, t := range tests {
		c.Check(getHostAddr(t.input), gocheck.Equals, t.expected)
	}
}

func (s *S) TestGetHostAddrWithSegregatedScheduler(c *gocheck.C) {
	conn, err := db.Conn()
	c.Assert(err, gocheck.IsNil)
	defer conn.Close()
	coll := conn.Collection(schedulerCollection)
	p := Pool{Name: "pool1", Nodes: []string{
		"http://remotehost:8080",
		"http://remotehost:8081",
		"http://10.10.10.1:8082",
	}}
	err = coll.Insert(p)
	defer coll.RemoveAll(bson.M{"_id": p.Name})
	cmutex.Lock()
	old := dCluster
	dCluster, err = cluster.New(segScheduler, &mapStorage{})
	c.Assert(err, gocheck.IsNil)
	cmutex.Unlock()
	defer func() {
		cmutex.Lock()
		dCluster = old
		cmutex.Unlock()
	}()
	var tests = []struct {
		input    string
		expected string
	}{
		{"http://remotehost:8080", "remotehost"},
		{"http://remotehost:8081", "remotehost"},
		{"http://10.10.10.1:8082", "10.10.10.1"},
		{"server33", ""},
	}
	for _, t := range tests {
		c.Check(getHostAddr(t.input), gocheck.Equals, t.expected)
	}
}

func (s *S) TestDockerCluster(c *gocheck.C) {
	config.Set("docker:servers", []string{"http://localhost:4243", "http://10.10.10.10:4243"})
	defer config.Unset("docker:servers")
	expected, _ := cluster.New(nil, nil,
		cluster.Node{ID: "server0", Address: "http://localhost:4243"},
		cluster.Node{ID: "server1", Address: "http://10.10.10.10:4243"},
	)
	nodes, err := dCluster.Nodes()
	c.Assert(err, gocheck.IsNil)
	cmutex.Lock()
	dCluster = nil
	cmutex.Unlock()
	defer func() {
		cmutex.Lock()
		defer cmutex.Unlock()
		dCluster, err = cluster.New(nil, &mapStorage{}, nodes...)
		c.Assert(err, gocheck.IsNil)
	}()
	cluster := dockerCluster()
	c.Assert(cluster, gocheck.DeepEquals, expected)
}

func (s *S) TestDockerClusterSegregated(c *gocheck.C) {
	config.Set("docker:segregate", true)
	defer config.Unset("docker:segregate")
	expected, _ := cluster.New(&segScheduler, nil)
	oldDockerCluster := dCluster
	cmutex.Lock()
	dCluster = nil
	cmutex.Unlock()
	defer func() {
		cmutex.Lock()
		defer cmutex.Unlock()
		dCluster = oldDockerCluster
	}()
	cluster := dockerCluster()
	c.Assert(cluster, gocheck.DeepEquals, expected)
}

func (s *S) TestGetDockerServersShouldSearchFromConfig(c *gocheck.C) {
	config.Set("docker:servers", []string{"http://server01.com:4243", "http://server02.com:4243"})
	defer config.Unset("docker:servers")
	servers := getDockerServers()
	expected := []cluster.Node{
		{ID: "server0", Address: "http://server01.com:4243"},
		{ID: "server1", Address: "http://server02.com:4243"},
	}
	c.Assert(servers, gocheck.DeepEquals, expected)
}

func (s *S) TestPushImage(c *gocheck.C) {
	var request *http.Request
	server, err := dtesting.NewServer("127.0.0.1:0", func(r *http.Request) {
		request = r
	})
	c.Assert(err, gocheck.IsNil)
	defer server.Stop()
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	var storage mapStorage
	storage.StoreImage("localhost:3030/base", "server0")
	cmutex.Lock()
	oldDockerCluster := dCluster
	dCluster, _ = cluster.New(nil, &storage,
		cluster.Node{ID: "server0", Address: server.URL()})
	cmutex.Unlock()
	defer func() {
		cmutex.Lock()
		defer cmutex.Unlock()
		dCluster = oldDockerCluster
	}()
	err = newImage("localhost:3030/base", "http://index.docker.io")
	c.Assert(err, gocheck.IsNil)
	err = pushImage("localhost:3030/base")
	c.Assert(err, gocheck.IsNil)
	c.Assert(request.URL.Path, gocheck.Matches, ".*/images/localhost:3030/base/push$")
}

func (s *S) TestPushImageNoRegistry(c *gocheck.C) {
	var request *http.Request
	server, err := dtesting.NewServer("127.0.0.1:0", func(r *http.Request) {
		request = r
	})
	c.Assert(err, gocheck.IsNil)
	defer server.Stop()
	err = pushImage("localhost:3030/base")
	c.Assert(err, gocheck.IsNil)
	c.Assert(request, gocheck.IsNil)
}

func (s *S) TestBuildImageName(c *gocheck.C) {
	repository := assembleImageName("raising")
	c.Assert(repository, gocheck.Equals, s.repoNamespace+"/raising")
}

func (s *S) TestBuildImageNameWithRegistry(c *gocheck.C) {
	config.Set("docker:registry", "localhost:3030")
	defer config.Unset("docker:registry")
	repository := assembleImageName("raising")
	expected := "localhost:3030/" + s.repoNamespace + "/raising"
	c.Assert(repository, gocheck.Equals, expected)
}

func (s *S) TestContainerStart(c *gocheck.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(cont)
	client, err := docker.NewClient(s.server.URL())
	c.Assert(err, gocheck.IsNil)
	dockerContainer, err := client.InspectContainer(cont.ID)
	c.Assert(err, gocheck.IsNil)
	c.Assert(dockerContainer.State.Running, gocheck.Equals, false)
	err = cont.start()
	c.Assert(err, gocheck.IsNil)
	dockerContainer, err = client.InspectContainer(cont.ID)
	c.Assert(err, gocheck.IsNil)
	c.Assert(dockerContainer.State.Running, gocheck.Equals, true)
	c.Assert(cont.Status, gocheck.Equals, provision.StatusStarted.String())
}

func (s *S) TestContainerStartWithoutPort(c *gocheck.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(cont)
	oldUser, _ := config.Get("docker:run-cmd:port")
	defer config.Set("docker:run-cmd:port", oldUser)
	config.Unset("docker:run-cmd:port")
	err = cont.start()
	c.Assert(err, gocheck.NotNil)
}

func (s *S) TestContainerStartStartedUnits(c *gocheck.C) {
	cont, err := s.newContainer(nil)
	c.Assert(err, gocheck.IsNil)
	defer s.removeTestContainer(cont)
	err = cont.start()
	c.Assert(err, gocheck.IsNil)
	err = cont.start()
	c.Assert(err, gocheck.NotNil)
}

func (s *S) TestUsePlatformImage(c *gocheck.C) {
	conn, err := db.Conn()
	c.Assert(err, gocheck.IsNil)
	defer conn.Close()
	app1 := &app.App{Name: "app1", Platform: "python", Deploys: 40}
	err = conn.Apps().Insert(app1)
	c.Assert(err, gocheck.IsNil)
	ok := usePlatformImage(app1)
	c.Assert(ok, gocheck.Equals, true)
	defer conn.Apps().Remove(bson.M{"name": "app1"})
	app2 := &app.App{Name: "app2", Platform: "python", Deploys: 20}
	err = conn.Apps().Insert(app2)
	c.Assert(err, gocheck.IsNil)
	ok = usePlatformImage(app2)
	c.Assert(ok, gocheck.Equals, true)
	defer conn.Apps().Remove(bson.M{"name": "app2"})
	app3 := &app.App{Name: "app3", Platform: "python", Deploys: 0}
	err = conn.Apps().Insert(app3)
	c.Assert(err, gocheck.IsNil)
	ok = usePlatformImage(app3)
	c.Assert(ok, gocheck.Equals, false)
	defer conn.Apps().Remove(bson.M{"name": "app3"})
	app4 := &app.App{Name: "app4", Platform: "python", Deploys: 19}
	err = conn.Apps().Insert(app4)
	c.Assert(err, gocheck.IsNil)
	ok = usePlatformImage(app4)
	c.Assert(ok, gocheck.Equals, false)
	defer conn.Apps().Remove(bson.M{"name": "app4"})
	app5 := &app.App{
		Name:           "app5",
		Platform:       "python",
		Deploys:        19,
		UpdatePlatform: true,
	}
	err = conn.Apps().Insert(app5)
	c.Assert(err, gocheck.IsNil)
	ok = usePlatformImage(app5)
	c.Assert(ok, gocheck.Equals, true)
	defer conn.Apps().Remove(bson.M{"name": "app5"})
}

func (s *S) TestContainerAvailable(c *gocheck.C) {
	cases := map[provision.Status]bool{
		provision.StatusStarted:     true,
		provision.StatusUnreachable: true,
		provision.StatusDown:        false,
		provision.StatusStopped:     false,
		provision.StatusBuilding:    false,
	}
	for status, expected := range cases {
		cont := container{Status: status.String()}
		c.Assert(cont.available(), gocheck.Equals, expected)
	}
}

func (s *S) TestUnitFromContainer(c *gocheck.C) {
	cont := container{
		ID:       "someid",
		AppName:  "someapp",
		Type:     "django",
		Status:   provision.StatusStarted.String(),
		HostAddr: "10.9.8.7",
	}
	expected := provision.Unit{
		Name:    cont.ID,
		AppName: cont.AppName,
		Type:    cont.Type,
		Status:  provision.Status(cont.Status),
		Ip:      cont.HostAddr,
	}
	c.Assert(unitFromContainer(cont), gocheck.Equals, expected)
}
