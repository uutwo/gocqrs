package gocqrs

import (
	"encoding/json"
	"errors"
	"github.com/dgrijalva/jwt-go"
	"github.com/diegogub/lib"
	"gopkg.in/gin-gonic/gin.v1"
	"log"
	"strconv"
	"sync"
	"time"
)

var (
	InvalidEntityError    = errors.New("Invalid entity")
	InvalidReferenceError = errors.New("Invalid reference")
)

const (
	EventTypeHeader     = "X-Event"
	EntityVersionHeader = "X-LockVersion"
	CreateEventHeader   = "X-Create"
	EntityHeader        = "X-Entity"
	EventIDHeader       = "X-EventID"
	SessionHeader       = "X-Session"
	CookieName          = "san"
)

var runningApp *App

type App struct {
	lock    sync.Mutex
	Version string `json:"version"`
	Name    string `json:"name"`
	Port    string `json:"port"`

	// user roles for auth
	Roles map[string]Role `json:"roles"`

	Entities map[string]*EntityConf `json:"entities"`
	Store    EventStore             `json:"-"`
	Router   *gin.Engine

	Sessions Sessioner `json:"-"`
	// turn off auth service check
	AuthOff         bool   `json:"authOff"`
	Secret          string `json:"-"`
	SessionValidity string `json:"sessionValidity"`
	sduration       time.Duration
	Domain          string   `json:"sessionDomain"`
	LoginReferers   []string `json:"loginReferers"`
}

func NewApp(store EventStore) *App {
	var app App
	app.Roles = make(map[string]Role)
	app.Entities = make(map[string]*EntityConf)
	app.Router = gin.New()
	app.Store = store
	// set default session validity
	app.SessionValidity = "5m"
	d, _ := time.ParseDuration(app.SessionValidity)
	app.sduration = d
	return &app
}

func (app *App) String() string {
	b, _ := json.Marshal(app)
	return string(b)
}

// Add Auth functionality
func (app *App) Auth(s Sessioner, evh ...EventHandler) {
	// Add user entity
	app.Sessions = s

	userEntity := NewEntityConf("user")
	userEntity.AddCRUD()

	for _, h := range evh {
		userEntity.AddEventHandler(h)
	}
	userEntity.AddEventHandler(UserEventHandler{})

	app.RegisterEntity(userEntity)
}

func (app *App) SessionTTL(d string) {
	sd, err := time.ParseDuration(d)
	if err != nil {
		log.Fatal(err)
	}
	app.SessionValidity = d
	app.sduration = sd
}

func (app *App) AddRoles(roles ...Role) {
	for _, r := range roles {
		app.Roles[r.Name] = r
	}
}

func (app *App) RegisterEntity(e *EntityConf) *App {
	_, has := app.Entities[e.Name]
	if !has {
		app.Entities[e.Name] = e
	} else {
		log.Fatal("Entity already added")
	}
	return app
}

func (app *App) HandleEvent(entityName, id string, ev Eventer, versionLock uint64) (string, uint64, error) {
	var err error
	app.lock.Lock()
	defer app.lock.Unlock()

	econf, ok := app.Entities[entityName]
	if !ok {
		return "", 0, InvalidEntityError
	}

	// look for entity events, TODO eventstore should cache streams
	stream := entityName + "-" + id
	ch, _ := app.Store.Range(stream)
	entity, err := econf.Aggregate(id, ch)
	if err != nil {
		return "", 0, err
	}

	h, has := econf.EventHandlers[ev.GetType()]
	if !has {
		return "", 0, errors.New("Invalid handler for event:" + ev.GetType())
	}

	// handler event
	opt, err := h.Handle(id, ev, entity)
	if err != nil {
		return "", 0, err
	}

	// check references
	for _, r := range econf.EntityReferences {
		var v string
		value := entity.Data[r.Key]
		switch value.(type) {
		case string:
			v = value.(string)
			err = app.CheckReference(r.Entity, r.Key, v, r.Null)
			if err != nil {
				return "", 0, err
			}
		case nil:
			if r.Null {
				return "", 0, errors.New("Invalid reference type, should be string")
			}
		default:
			return "", 0, errors.New("Invalid reference type, should be string")
		}
	}

	//validate entity
	for n, v := range econf.Validators {
		err = v.Validate(*entity)
		if err != nil {
			return "", 0, errors.New("Failed validation: " + n + " - " + err.Error())
		}
	}

	version, err := app.Store.Store(ev, opt)
	return entity.ID, version, err
}

// Start app
func (app *App) Run(port string) error {
	app.Router.POST("/event/:entity", HTTPEventHandler)
	app.Router.GET("/docs", DocHandler)
	app.Router.GET("/entity/:entity/:id", EntityHandler)
	app.Router.POST("/auth", AuthHandler)
	app.Router.POST("/session/renew", AuthRenewHandler)
	runningApp = app
	return runningApp.Router.Run(port)
}

type SessionClaims struct {
	Username string `json:"use"`
	Role     string `json:"rol"`
	jwt.StandardClaims
}

func AuthHandler(c *gin.Context) {
	// AUTH USER
	var u User
	username := c.PostForm("u")
	password := c.PostForm("p")
	t := c.PostForm("t")

	e, _, err := runningApp.Entity("user", username)
	if err != nil {
		c.JSON(401, map[string]string{"error": "Failed to login"})
		return
	}
	e.Decode(&u)
	err = u.CheckPassword(password)
	if err != nil {
		c.JSON(401, map[string]string{"error": "Failed to login"})
		return
	}

	allowedReferer := false
	// I have to check referers to login, or ask for user token
	for _, r := range runningApp.LoginReferers {
		if r == c.Request.Referer() {
			allowedReferer = true
			break
		}
	}

	if t != "" {
		err = u.CheckToken(t)
		if err != nil {
			c.JSON(401, map[string]string{"error": "Failed to login"})
			return
		}
	}

	if !allowedReferer {
		//TODO CHECK IP
	}

	// Create token with basic user data
	claims := SessionClaims{
		u.Username,
		u.Role,
		jwt.StandardClaims{
			IssuedAt:  time.Now().Unix(),
			ExpiresAt: time.Now().Add(runningApp.sduration).Unix(),
			Issuer:    runningApp.Name,
			Id:        lib.NewShortId(""),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	// Sign and get the complete encoded token as a string using the secret
	tokenString, _ := token.SignedString([]byte(runningApp.Secret))

	c.SetCookie("cs", tokenString, -1, "/", runningApp.Domain, false, true)
	c.JSON(200, map[string]string{"auth-token": tokenString})
}

func AuthRenewHandler(c *gin.Context) {
	// Renew session
}

func HTTPEventHandler(c *gin.Context) {
	var err error
	e := c.Param("entity")
	eType := c.Request.Header.Get(EventTypeHeader)

	data := make(map[string]interface{})

	if !runningApp.AuthOff {
		_, err = runningApp.auth(eType, c)
		if err != nil {
			c.JSON(401, map[string]interface{}{"error": err.Error()})
			return
		}
	}

	err = c.BindJSON(&data)
	if err != nil {
		c.JSON(400, map[string]interface{}{"error": err.Error()})
		return
	}

	eVersion := c.Request.Header.Get(EntityVersionHeader)
	v, _ := strconv.ParseUint(eVersion, 10, 64)

	// get event type

	// get entity id
	enID := c.Request.Header.Get(EntityHeader)
	if enID == "" {
		enID = lib.NewShortId("")
	}

	// get event id
	eID := c.Request.Header.Get(EventIDHeader)

	event := NewEvent(eID, eType, data)
	event.Entity = e
	event.EntityID = enID
	log.Println(event)

	// create event
	id, version, err := runningApp.HandleEvent(event.Entity, event.EntityID, event, v)
	if err != nil {
		c.JSON(400, map[string]interface{}{"error": err.Error()})
		return
	}

	c.JSON(201, map[string]interface{}{"entity": e, "entity-id": enID, "version": version, "event-id": id})
	return
}

func DocHandler(c *gin.Context) {
	c.JSON(200, GenerateDocs(runningApp))
}

func EntityHandler(c *gin.Context) {
	e := c.Param("entity")
	id := c.Param("id")
	entity, _, err := runningApp.Entity(e, id)
	if err != nil {
		c.JSON(400, map[string]string{"error": err.Error()})
		return
	}

	c.JSON(200, entity)
}

func (app *App) Entity(name, id string) (*Entity, uint64, error) {
	econf, ok := app.Entities[name]
	if !ok {
		return nil, 0, errors.New("Invalid entity name")
	}

	// look for entity events, TODO eventstore should cache streams
	stream := name + "-" + id
	ch, version := app.Store.Range(stream)
	entity, err := econf.Aggregate(id, ch)
	if err != nil {
		return nil, 0, err
	}
	entity.Version = version

	return entity, version, err
}

func (app *App) authRole(eventType, role string) bool {
	allowed := false
	for _, r := range app.Roles {
		if r.Name == role {
			allowed = r.Can(eventType)
			break
		}
	}
	return allowed
}

func (app *App) CheckReference(e, k, value string, null bool) error {
	if value == "" && null {
		return nil
	}

	stream := e + "-" + value
	_, err := app.Store.Version(stream)
	if err != nil {
		return errors.New(InvalidReferenceError.Error() + ": " + k + " - " + value + " - " + stream + " - " + err.Error())
	}

	return err
}

func (app *App) auth(event string, c *gin.Context) (*SessionClaims, error) {
	var err error
	t := ""
	// Read cookie
	cookieVal, err := c.Cookie(CookieName)
	if cookieVal == "" {
		t = c.Request.Header.Get(SessionHeader)
	} else {
		t = cookieVal
	}

	// create session claimsfrom token
	token, err := jwt.ParseWithClaims(t, &SessionClaims{}, func(token *jwt.Token) (interface{}, error) {
		return []byte(app.Secret), nil
	})

	if token == nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*SessionClaims); ok && token.Valid {
		if !app.authRole(claims.Role, event) {
			return nil, errors.New("Invalid Role")
		}

		return claims, err
	} else {
		return nil, err
	}

}
