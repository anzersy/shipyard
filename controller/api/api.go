package api

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/codegangsta/negroni"
	"github.com/gorilla/context"
	"github.com/gorilla/mux"
	"github.com/mailgun/oxy/forward"
	"github.com/samalba/dockerclient"
	"github.com/shipyard/shipyard"
	"github.com/shipyard/shipyard/auth"
	"github.com/shipyard/shipyard/controller/manager"
	"github.com/shipyard/shipyard/controller/middleware/access"
	mAuth "github.com/shipyard/shipyard/controller/middleware/auth"
	"github.com/shipyard/shipyard/dockerhub"
	"golang.org/x/net/websocket"
)

type (
	Api struct {
		listenAddr         string
		manager            manager.Manager
		authWhitelistCIDRs []string
		enableCors         bool
		serverVersion      string
	}

	Credentials struct {
		Username string `json:"username,omitempty"`
		Password string `json:"password,omitempty"`
	}
)

func writeCorsHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")
	w.Header().Add("Access-Control-Allow-Methods", "GET, POST, DELETE, PUT, OPTIONS")
}

func NewApi(listenAddr string, manager manager.Manager, authWhitelistCIDRs []string, enableCors bool) (*Api, error) {
	return &Api{
		listenAddr:         listenAddr,
		manager:            manager,
		authWhitelistCIDRs: authWhitelistCIDRs,
		enableCors:         enableCors,
	}, nil
}

func (a *Api) addServiceKey(w http.ResponseWriter, r *http.Request) {
	var k *auth.ServiceKey
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	key, err := a.manager.NewServiceKey(k.Description)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Infof("created service key key=%s description=%s", key.Key, key.Description)
	if err := json.NewEncoder(w).Encode(key); err != nil {
		log.Error(err)
	}
}

func (a *Api) serviceKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	keys, err := a.manager.ServiceKeys()
	if err != nil {
		log.Error(err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := json.NewEncoder(w).Encode(keys); err != nil {
		log.Error(err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
}

func (a *Api) removeServiceKey(w http.ResponseWriter, r *http.Request) {
	var key *auth.ServiceKey
	if err := json.NewDecoder(r.Body).Decode(&key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.manager.RemoveServiceKey(key.Key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Infof("removed service key %s", key.Key)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Api) registries(w http.ResponseWriter, r *http.Request) {
	registries, err := a.manager.Registries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := json.NewEncoder(w).Encode(registries); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) addRegistry(w http.ResponseWriter, r *http.Request) {
	var registry *shipyard.Registry
	if err := json.NewDecoder(r.Body).Decode(&registry); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := a.manager.AddRegistry(registry); err != nil {
		log.Errorf("error saving registry: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Infof("added registry: name=%s", registry.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Api) registry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	vars := mux.Vars(r)
	name := vars["name"]

	registry, err := a.manager.Registry(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := json.NewEncoder(w).Encode(registry); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) removeRegistry(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]

	registry, err := a.manager.Registry(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := a.manager.RemoveRegistry(registry); err != nil {
		log.Errorf("error deleting registry: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) repositories(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	vars := mux.Vars(r)
	name := vars["name"]

	registry, err := a.manager.Registry(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	repos, err := registry.Repositories()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(repos); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) repository(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	vars := mux.Vars(r)
	name := vars["name"]
	repoName := vars["repo"]

	registry, err := a.manager.Registry(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	repo, err := registry.Repository(repoName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(repo); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) deleteRepository(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]
	repoName := vars["repo"]

	registry, err := a.manager.Registry(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := registry.DeleteRepository(repoName); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (a *Api) inspectRepository(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	name := vars["name"]
	repoName := vars["repo"]

	registry, err := a.manager.Registry(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	repo, err := registry.Repository(repoName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(repo); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	limit := -1
	l := r.FormValue("limit")
	if l != "" {
		lt, err := strconv.Atoi(l)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		limit = lt
	}
	events, err := a.manager.Events(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(events); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) purgeEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	if err := a.manager.PurgeEvents(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Info("cluster events purged")
	w.WriteHeader(http.StatusNoContent)
}

func (a *Api) accounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	accounts, err := a.manager.Accounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(accounts); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) addAccount(w http.ResponseWriter, r *http.Request) {
	var account *auth.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := a.manager.SaveAccount(account); err != nil {
		log.Errorf("error saving account: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Infof("saved account %s", account.Username)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Api) deleteAccount(w http.ResponseWriter, r *http.Request) {
	var acct *auth.Account
	if err := json.NewDecoder(r.Body).Decode(&acct); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	account, err := a.manager.Account(acct.Username)
	if err != nil {
		log.Errorf("error deleting account: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.manager.DeleteAccount(account); err != nil {
		log.Errorf("error deleting account: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Infof("deleted account %s (%s)", account.Username, account.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Api) roles(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	roles, err := a.manager.Roles()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(roles); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) role(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	vars := mux.Vars(r)
	name := vars["name"]
	role, err := a.manager.Role(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(role); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) addRole(w http.ResponseWriter, r *http.Request) {
	var role *auth.Role
	if err := json.NewDecoder(r.Body).Decode(&role); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := a.manager.SaveRole(role); err != nil {
		log.Errorf("error saving role: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Infof("saved role %s", role.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Api) deleteRole(w http.ResponseWriter, r *http.Request) {
	var role *auth.Role
	if err := json.NewDecoder(r.Body).Decode(&role); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.manager.DeleteRole(role); err != nil {
		log.Errorf("error deleting role: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) webhookKeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	keys, err := a.manager.WebhookKeys()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(keys); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) webhookKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	vars := mux.Vars(r)
	id := vars["id"]
	key, err := a.manager.WebhookKey(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) addWebhookKey(w http.ResponseWriter, r *http.Request) {
	var k *dockerhub.WebhookKey
	if err := json.NewDecoder(r.Body).Decode(&k); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	key, err := a.manager.NewWebhookKey(k.Image)
	if err != nil {
		log.Errorf("error generating webhook key: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Infof("saved webhook key image=%s", key.Image)
	if err := json.NewEncoder(w).Encode(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) deleteWebhookKey(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]
	if err := a.manager.DeleteWebhookKey(id); err != nil {
		log.Errorf("error deleting webhook key: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Infof("removed webhook key id=%s", id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *Api) login(w http.ResponseWriter, r *http.Request) {
	var creds *Credentials
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !a.manager.Authenticate(creds.Username, creds.Password) {
		log.Errorf("invalid login for %s from %s", creds.Username, r.RemoteAddr)
		http.Error(w, "invalid username/password", http.StatusForbidden)
		return
	}
	// return token
	token, err := a.manager.NewAuthToken(creds.Username, r.UserAgent())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(token); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) changePassword(w http.ResponseWriter, r *http.Request) {
	session, _ := a.manager.Store().Get(r, a.manager.StoreKey())
	var creds *Credentials
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	username := session.Values["username"].(string)
	if username == "" {
		http.Error(w, "unauthorized", http.StatusInternalServerError)
		return
	}
	if err := a.manager.ChangePassword(username, creds.Password); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) hubWebhook(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]
	key, err := a.manager.WebhookKey(id)
	if err != nil {
		log.Errorf("invalid webook key: id=%s from %s", id, r.RemoteAddr)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var webhook *dockerhub.Webhook
	if err := json.NewDecoder(r.Body).Decode(&webhook); err != nil {
		log.Errorf("error parsing webhook: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if strings.Index(webhook.Repository.RepoName, key.Image) == -1 {
		log.Errorf("webhook key image does not match: repo=%s image=%s", webhook.Repository.RepoName, key.Image)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	log.Infof("received webhook notification for %s", webhook.Repository.RepoName)
	// TODO @ehazlett - redeploy containers
}

func (a *Api) nodes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	nodes, err := a.manager.Nodes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(nodes); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) node(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")

	vars := mux.Vars(r)
	name := vars["name"]
	node, err := a.manager.Node(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(node); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *Api) execContainer(ws *websocket.Conn) {
	qry := ws.Request().URL.Query()
	containerId := qry.Get("id")
	command := qry.Get("cmd")
	ttyWidth := qry.Get("w")
	ttyHeight := qry.Get("h")
	cmd := strings.Split(command, ",")

	log.Debugf("starting exec session: container=%s cmd=%s", containerId, command)
	clientUrl := a.manager.DockerClient().URL
	host := fmt.Sprintf("%s://%s",
		clientUrl.Scheme,
		clientUrl.Host)

	execConfig := &dockerclient.ExecConfig{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          cmd,
	}

	buf, err := json.Marshal(execConfig)
	if err != nil {
		log.Error(err)
		return
	}

	rdr := bytes.NewReader(buf)

	u := fmt.Sprintf("%s/containers/%s/exec", host, containerId)

	resp, err := http.Post(u, "application/json", rdr)
	if err != nil {
		log.Error(err)
		return
	}
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
		return
	}

	var info dockerclient.ContainerInfo
	json.Unmarshal([]byte(data), &info)

	if err := a.hijack(clientUrl.Host, "POST", "/exec/"+info.Id+"/start", true, ws, ws, ws, nil, nil); err != nil {
		log.Errorf("error during hijack: %s", err)
		return
	}

	// resize
	u = fmt.Sprintf("%s/exec/%s/resize?w=%s&h=%s", host, info.Id, ttyWidth, ttyHeight)

	resp, err = http.Post(u, "application/json", nil)
	if err != nil {
		log.Error(err)
		return
	}
}

func (a *Api) hijack(addr, method, path string, setRawTerminal bool, in io.ReadCloser, stdout, stderr io.Writer, started chan io.Closer, data interface{}) error {
	execConfig := &dockerclient.ExecConfig{
		Tty:    true,
		Detach: false,
	}

	buf, err := json.Marshal(execConfig)
	if err != nil {
		return err
	}

	rdr := bytes.NewReader(buf)

	req, err := http.NewRequest(method, path, rdr)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", "Docker-Client")
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	req.Host = addr

	var (
		dial      net.Conn
		dialErr   error
		tlsConfig = a.manager.DockerClient().TLSConfig
	)

	if tlsConfig == nil {
		dial, dialErr = net.Dial("tcp", addr)
	} else {
		dial, dialErr = tls.Dial("tcp", addr, tlsConfig)
	}

	if dialErr != nil {
		return dialErr
	}

	// When we set up a TCP connection for hijack, there could be long periods
	// of inactivity (a long running command with no output) that in certain
	// network setups may cause ECONNTIMEOUT, leaving the client in an unknown
	// state. Setting TCP KeepAlive on the socket connection will prohibit
	// ECONNTIMEOUT unless the socket connection truly is broken
	if tcpConn, ok := dial.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
	if err != nil {
		return err
	}
	clientconn := httputil.NewClientConn(dial, nil)
	defer clientconn.Close()

	// Server hijacks the connection, error 'connection closed' expected
	clientconn.Do(req)

	rwc, br := clientconn.Hijack()
	defer rwc.Close()

	if started != nil {
		started <- rwc
	}

	var receiveStdout chan error

	if stdout != nil || stderr != nil {
		go func() (err error) {
			if setRawTerminal && stdout != nil {
				_, err = io.Copy(stdout, br)
			}
			return err
		}()
	}

	go func() error {
		if in != nil {
			io.Copy(rwc, in)
		}

		if conn, ok := rwc.(interface {
			CloseWrite() error
		}); ok {
			if err := conn.CloseWrite(); err != nil {
			}
		}
		return nil
	}()

	if stdout != nil || stderr != nil {
		if err := <-receiveStdout; err != nil {
			return err
		}
	}
	go func() {
		for {
			fmt.Println(br)
		}
	}()

	return nil
}

func (a *Api) Run() error {
	globalMux := http.NewServeMux()
	controllerManager := a.manager
	client := a.manager.DockerClient()

	// forwarder for swarm
	fwd, err := forward.New()
	if err != nil {
		return err
	}

	u := client.URL

	// setup redirect target to swarm
	scheme := "http://"

	// check if TLS is enabled and configure if so
	if client.TLSConfig != nil {
		scheme = "https://"
		// setup custom roundtripper with TLS transport
		r := forward.RoundTripper(
			&http.Transport{
				TLSClientConfig: client.TLSConfig,
			})
		f, err := forward.New(r)
		if err != nil {
			log.Fatal(err)
		}

		fwd = f
	}

	dUrl := fmt.Sprintf("%s%s", scheme, u.Host)

	log.Debugf("configured docker proxy target: %s", dUrl)

	swarmRedirect := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.URL, err = url.ParseRequestURI(dUrl)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		fwd.ServeHTTP(w, req)
	})

	apiRouter := mux.NewRouter()
	apiRouter.HandleFunc("/api/accounts", a.accounts).Methods("GET")
	apiRouter.HandleFunc("/api/accounts", a.addAccount).Methods("POST")
	apiRouter.HandleFunc("/api/accounts", a.deleteAccount).Methods("DELETE")
	apiRouter.HandleFunc("/api/roles", a.roles).Methods("GET")
	apiRouter.HandleFunc("/api/roles/{name}", a.role).Methods("GET")
	apiRouter.HandleFunc("/api/roles", a.addRole).Methods("POST")
	apiRouter.HandleFunc("/api/roles", a.deleteRole).Methods("DELETE")
	apiRouter.HandleFunc("/api/nodes", a.nodes).Methods("GET")
	apiRouter.HandleFunc("/api/nodes/{name}", a.node).Methods("GET")
	apiRouter.HandleFunc("/api/events", a.events).Methods("GET")
	apiRouter.HandleFunc("/api/events", a.purgeEvents).Methods("DELETE")
	apiRouter.HandleFunc("/api/registry", a.registries).Methods("GET")
	apiRouter.HandleFunc("/api/registry", a.addRegistry).Methods("POST")
	apiRouter.HandleFunc("/api/registry/{name}", a.registry).Methods("GET")
	apiRouter.HandleFunc("/api/registry/{name}", a.removeRegistry).Methods("DELETE")
	apiRouter.HandleFunc("/api/registry/{name}/repositories", a.repositories).Methods("GET")
	apiRouter.HandleFunc("/api/registry/{name}/repositories/{repo:.*}", a.repository).Methods("GET")
	apiRouter.HandleFunc("/api/registry/{name}/repositories/{repo:.*}", a.deleteRepository).Methods("DELETE")
	apiRouter.HandleFunc("/api/servicekeys", a.serviceKeys).Methods("GET")
	apiRouter.HandleFunc("/api/servicekeys", a.addServiceKey).Methods("POST")
	apiRouter.HandleFunc("/api/servicekeys", a.removeServiceKey).Methods("DELETE")
	apiRouter.HandleFunc("/api/webhookkeys", a.webhookKeys).Methods("GET")
	apiRouter.HandleFunc("/api/webhookkeys/{id}", a.webhookKey).Methods("GET")
	apiRouter.HandleFunc("/api/webhookkeys", a.addWebhookKey).Methods("POST")
	apiRouter.HandleFunc("/api/webhookkeys/{id}", a.deleteWebhookKey).Methods("DELETE")
	//apiRouter.HandleFunc("/api/exec/{id}/{cmd:.*}", a.execContainer)
	apiRouter.Handle("/api/exec", websocket.Handler(a.execContainer))

	// global handler
	globalMux.Handle("/", http.FileServer(http.Dir("static")))

	// api router; protected by auth
	apiAuthRouter := negroni.New()
	apiAuthRequired := mAuth.NewAuthRequired(controllerManager, a.authWhitelistCIDRs)
	apiAccessRequired := access.NewAccessRequired(controllerManager)
	apiAuthRouter.Use(negroni.HandlerFunc(apiAuthRequired.HandlerFuncWithNext))
	apiAuthRouter.Use(negroni.HandlerFunc(apiAccessRequired.HandlerFuncWithNext))
	apiAuthRouter.UseHandler(apiRouter)
	globalMux.Handle("/api/", apiAuthRouter)

	// account router ; protected by auth
	accountRouter := mux.NewRouter()
	accountRouter.HandleFunc("/account/changepassword", a.changePassword).Methods("POST")
	accountAuthRouter := negroni.New()
	accountAuthRequired := mAuth.NewAuthRequired(controllerManager, a.authWhitelistCIDRs)
	accountAuthRouter.Use(negroni.HandlerFunc(accountAuthRequired.HandlerFuncWithNext))
	accountAuthRouter.UseHandler(accountRouter)
	globalMux.Handle("/account/", accountAuthRouter)

	// login handler; public
	loginRouter := mux.NewRouter()
	loginRouter.HandleFunc("/auth/login", a.login).Methods("POST")
	globalMux.Handle("/auth/", loginRouter)

	// hub handler; public
	hubRouter := mux.NewRouter()
	hubRouter.HandleFunc("/hub/webhook/{id}", a.hubWebhook).Methods("POST")
	globalMux.Handle("/hub/", hubRouter)

	// swarm
	swarmRouter := mux.NewRouter()
	// these are pulled from the swarm api code to proxy and allow
	// usage with the standard Docker cli
	m := map[string]map[string]http.HandlerFunc{
		"GET": {
			"/_ping":                          swarmRedirect,
			"/events":                         swarmRedirect,
			"/info":                           swarmRedirect,
			"/version":                        swarmRedirect,
			"/images/json":                    swarmRedirect,
			"/images/viz":                     swarmRedirect,
			"/images/search":                  swarmRedirect,
			"/images/get":                     swarmRedirect,
			"/images/{name:.*}/get":           swarmRedirect,
			"/images/{name:.*}/history":       swarmRedirect,
			"/images/{name:.*}/json":          swarmRedirect,
			"/containers/ps":                  swarmRedirect,
			"/containers/json":                swarmRedirect,
			"/containers/{name:.*}/export":    swarmRedirect,
			"/containers/{name:.*}/changes":   swarmRedirect,
			"/containers/{name:.*}/json":      swarmRedirect,
			"/containers/{name:.*}/top":       swarmRedirect,
			"/containers/{name:.*}/logs":      swarmRedirect,
			"/containers/{name:.*}/stats":     swarmRedirect,
			"/containers/{name:.*}/attach/ws": swarmRedirect,
			"/exec/{execid:.*}/json":          swarmRedirect,
		},
		"POST": {
			"/auth":                         swarmRedirect,
			"/commit":                       swarmRedirect,
			"/build":                        swarmRedirect,
			"/images/create":                swarmRedirect,
			"/images/load":                  swarmRedirect,
			"/images/{name:.*}/push":        swarmRedirect,
			"/images/{name:.*}/tag":         swarmRedirect,
			"/containers/create":            swarmRedirect,
			"/containers/{name:.*}/kill":    swarmRedirect,
			"/containers/{name:.*}/pause":   swarmRedirect,
			"/containers/{name:.*}/unpause": swarmRedirect,
			"/containers/{name:.*}/rename":  swarmRedirect,
			"/containers/{name:.*}/restart": swarmRedirect,
			"/containers/{name:.*}/start":   swarmRedirect,
			"/containers/{name:.*}/stop":    swarmRedirect,
			"/containers/{name:.*}/wait":    swarmRedirect,
			"/containers/{name:.*}/resize":  swarmRedirect,
			"/containers/{name:.*}/attach":  swarmRedirect,
			"/containers/{name:.*}/copy":    swarmRedirect,
			"/containers/{name:.*}/exec":    swarmRedirect,
			"/exec/{execid:.*}/start":       swarmRedirect,
			"/exec/{execid:.*}/resize":      swarmRedirect,
		},
		"DELETE": {
			"/containers/{name:.*}": swarmRedirect,
			"/images/{name:.*}":     swarmRedirect,
		},
		"OPTIONS": {
			"": swarmRedirect,
		},
	}

	for method, routes := range m {
		for route, fct := range routes {
			localRoute := route
			localFct := fct
			wrap := func(w http.ResponseWriter, r *http.Request) {
				if a.enableCors {
					writeCorsHeaders(w, r)
				}
				localFct(w, r)
			}
			localMethod := method

			// add the new route
			swarmRouter.Path("/v{version:[0-9.]+}" + localRoute).Methods(localMethod).HandlerFunc(wrap)
			swarmRouter.Path(localRoute).Methods(localMethod).HandlerFunc(wrap)
		}
	}

	swarmAuthRouter := negroni.New()
	swarmAuthRequired := mAuth.NewAuthRequired(controllerManager, a.authWhitelistCIDRs)
	swarmAccessRequired := access.NewAccessRequired(controllerManager)
	swarmAuthRouter.Use(negroni.HandlerFunc(swarmAuthRequired.HandlerFuncWithNext))
	swarmAuthRouter.Use(negroni.HandlerFunc(swarmAccessRequired.HandlerFuncWithNext))
	swarmAuthRouter.UseHandler(swarmRouter)
	globalMux.Handle("/containers/", swarmAuthRouter)
	globalMux.Handle("/_ping", swarmAuthRouter)
	globalMux.Handle("/commit", swarmAuthRouter)
	globalMux.Handle("/build", swarmAuthRouter)
	globalMux.Handle("/events", swarmAuthRouter)
	globalMux.Handle("/version", swarmAuthRouter)
	globalMux.Handle("/images/", swarmAuthRouter)
	globalMux.Handle("/exec/", swarmAuthRouter)
	globalMux.Handle("/v1.17/", swarmAuthRouter)
	globalMux.Handle("/v1.18/", swarmAuthRouter)

	// check for admin user
	if _, err := controllerManager.Account("admin"); err == manager.ErrAccountDoesNotExist {
		// create roles
		r := &auth.Role{
			Name: "admin",
		}
		ru := &auth.Role{
			Name: "user",
		}
		if err := controllerManager.SaveRole(r); err != nil {
			log.Fatal(err)
		}
		if err := controllerManager.SaveRole(ru); err != nil {
			log.Fatal(err)
		}
		role, err := controllerManager.Role(r.Name)
		if err != nil {
			log.Fatal(err)
		}
		acct := &auth.Account{
			Username: "admin",
			Password: "shipyard",
			Role:     role,
		}
		if err := controllerManager.SaveAccount(acct); err != nil {
			log.Fatal(err)
		}
		log.Infof("created admin user: username: admin password: shipyard")
	}

	log.Infof("controller listening on %s", a.listenAddr)

	s := &http.Server{
		Addr:    a.listenAddr,
		Handler: context.ClearHandler(globalMux),
	}

	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}

	return http.ListenAndServe(a.listenAddr, context.ClearHandler(globalMux))
}