/*
	This is a database interface library using Exasol's websocket API
    https://github.com/exasol/websocket-api/blob/master/WebsocketAPI.md

	TODOs:
	1) Support connection compression
	2) Convert to database/sql interface


	AUTHOR

	Grant Street Group <developers@grantstreet.com>

	COPYRIGHT AND LICENSE

	This software is Copyright (c) 2019 by Grant Street Group.
	This is free software, licensed under:
	    MIT License
*/

package exasol

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/url"
	"os/user"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"time"
)

/*--- Public Interface ---*/

const ExasolAPIVersion = 1
const DriverVersion = "2"

type ConnConf struct {
	Host           string
	Port           uint16
	Username       string
	Password       string
	ClientName     string
	ClientVersion  string
	ConnectTimeout time.Duration
	QueryTimeout   time.Duration
	TLSConfig      *tls.Config
	SuppressError  bool // Server errors are logged to Error by default
	// TODO try compressionEnabled: true
	Logger         Logger    // Optional for better control over logging
	WSHandler      WSHandler // Optional for intercepting websocket traffic
	CachePrepStmts bool

	FetchReqSize int

	Timeout uint32 // Deprecated - Use Query/ConnectTimeout instead
}

// By default we use the gorilla/websocket implementation however you can also
// specify a custom websocket handler which you can then use to intercept
// API traffic. This is handy for:
//   1. Using a non-gorilla websocket library
//   2. Emulating Exasol for testing purposes
//   3. Intercepting and manipulating the traffic (e.g. for buffering, caching etc)
// See websocket_handler.go for the default implementation.
// The custom websocket handler must conform to the following interface:
type WSHandler interface {
	// tls.Config is optional. If specified SSL should be enabled
	// time.Duration is the connect timeout (or zero for none)
	Connect(url.URL, *tls.Config, time.Duration) error
	EnableCompression(bool)
	// Write/ReadJSON will be passed structs from api.go
	WriteJSON(interface{}) error
	ReadJSON(interface{}) error
	Close()
}

type Conn struct {
	Conf      ConnConf
	SessionID uint64
	Stats     map[string]int
	Metadata  *AuthData

	log           Logger
	wsh           WSHandler
	prepStmtCache map[string]*prepStmt
	mux           sync.Mutex
	ctx           context.Context
	fetchReqSize  int
}

type FetchResult struct {
	Data  []interface{}
	Error error
}

func Connect(conf ConnConf) (*Conn, error) {
	return ConnectContext(conf, context.TODO())

}

func ConnectContext(conf ConnConf, ctx context.Context) (*Conn, error) {
	c := &Conn{
		Conf:          conf,
		Stats:         map[string]int{},
		log:           conf.Logger,
		wsh:           conf.WSHandler,
		prepStmtCache: map[string]*prepStmt{},
		ctx:           ctx,
		fetchReqSize:  conf.FetchReqSize,
	}

	if c.Conf.FetchReqSize <= 0 || c.Conf.FetchReqSize > 64*1024*1024 {
		c.Conf.FetchReqSize = 64 * 1024 * 1024
	}

	if c.Conf.Timeout > 0 {
		c.log.Warning("exasol.ConnConf.Timeout option is deprecated. Use QueryTimeout instead.")
		c.Conf.QueryTimeout = time.Duration(c.Conf.Timeout) * time.Second
	}

	if c.log == nil {
		c.log = newDefaultLogger()
	}

	if c.wsh == nil {
		c.wsh = newDefaultWSHandler()
	}

	err := c.wsConnect()
	if err != nil {
		return nil, c.errorf("Unable to connect to Exasol: %w", err)
	}

	err = c.login()
	if err != nil {
		return nil, c.errorf("Unable to login to Exasol: %s", err)
	}

	return c, nil
}

func (c *Conn) Disconnect() {
	c.log.Info("Disconnecting SessionID:", c.SessionID)

	for _, ps := range c.prepStmtCache {
		c.closePrepStmt(ps.sth)
	}
	err := c.send(&request{Command: "disconnect"}, &response{})
	if err != nil {
		c.log.Warning("Unable to disconnect from Exasol: ", err)
	}
	c.wsh.Close()
	c.wsh = nil
}

func (c *Conn) GetSessionAttr() (*Attributes, error) {
	req := &request{Command: "getAttributes"}
	res := &response{}
	err := c.send(req, res)
	if err != nil {
		return nil, c.errorf("Unable to get session attributes: %s", err)
	}
	return res.Attributes, nil
}

func (c *Conn) EnableAutoCommit() error {
	c.log.Info("Enabling AutoCommit")
	err := c.send(&request{
		Command:    "setAttributes",
		Attributes: &Attributes{Autocommit: true},
	}, &response{})
	if err != nil {
		return c.errorf("Unable to enable autocommit: %s", err)
	}
	return nil
}

func (c *Conn) DisableAutoCommit() error {
	c.log.Info("Disabling AutoCommit")
	// We have to roll our own map because Attributes
	// needs to have AutoCommit set to omitempty which
	// causes autocommit=false not to be sent :-(
	err := c.send(map[string]interface{}{
		"command": "setAttributes",
		"attributes": map[string]interface{}{
			"autocommit": false,
		},
	}, &response{})
	if err != nil {
		return c.errorf("Unable to disable autocommit: %s", err)
	}
	return nil
}

func (c *Conn) Rollback() error {
	c.log.Info("Rolling back transaction")
	_, err := c.execute("ROLLBACK", nil, "", nil, false)
	if err != nil {
		return c.errorf("Unable to rollback: %s", err)
	}
	return nil
}

func (c *Conn) Commit() error {
	c.log.Info("Committing transaction")
	_, err := c.execute("COMMIT", nil, "", nil, false)
	if err != nil {
		return c.errorf("Unable to commit: %s", err)
	}
	return nil
}

// TODO change optional args into an ExecConf struct
// Optional args are binds, default schema, colDefs, isColumnar flag
// 1) The binds are data bindings for statements containing placeholders.
//    You can either specify it as []interface{} if there's only one row
//    or as [][]interface{} if there are multiple rows.
// 2) Specifying the default schema allows you to use non-schema-qualified
//    table identifiers in the statement even when you have no schema currently open.
// 3) The colDefs option expects a []DataTypes. This is only necessary if you are
//    working around a bug that existed in pre-v6.0.9 of Exasol
//    (https://www.exasol.com/support/browse/EXASOL-2138)
// 4) The isColumnar boolean indicates whether the binds specified in the
//    first optional arg are in columnar format (By default the are in row format.)
func (c *Conn) Execute(sql string, args ...interface{}) (rowsAffected int64, err error) {
	var binds [][]interface{}
	if len(args) > 0 && args[0] != nil {
		switch b := args[0].(type) {
		case [][]interface{}:
			binds = b
		case []interface{}:
			binds = append(binds, b)
		default:
			return 0, c.error("Execute's 2nd param (binds) must be []interface{} or [][]interface{}")
		}
	}
	var schema string
	if len(args) > 1 && args[1] != nil {
		switch s := args[1].(type) {
		case string:
			schema = s
		default:
			return 0, c.error("Execute's 3nd param (schema) must be a string")
		}
	}
	var dataTypes []DataType
	if len(args) > 2 && args[2] != nil {
		switch d := args[2].(type) {
		case []DataType:
			dataTypes = d
		default:
			return 0, c.error("Execute's 4th param (data types) must be a []DataType")
		}
	}
	isColumnar := false // Whether or not the passed-in binds are columnar
	if len(args) > 3 && args[3] != nil {
		switch ic := args[3].(type) {
		case bool:
			isColumnar = ic
		default:
			return 0, c.error("Execute's 5th param (isColumnar) must be a boolean")
		}
	}

	res, err := c.execute(sql, binds, schema, dataTypes, isColumnar)
	if err != nil {
		return 0, c.errorf("Unable to Execute: %s", err)
	} else if res.ResponseData.NumResults > 0 {
		return res.ResponseData.Results[0].RowCount, nil
	}
	return 0, nil
}

// Optional args are binds, and default schema
// 1) The binds are data bindings for queries containing placeholders.
//    You can specify it []interface{}
// 2) Specifying the default schema allows you to use non-schema-qualified
//    table identifiers in the statement even when you have no schema currently open.
func (c *Conn) FetchChan(sql string, args ...interface{}) (<-chan FetchResult, error) {
	var binds []interface{}
	if len(args) > 0 && args[0] != nil {
		switch b := args[0].(type) {
		case []interface{}:
			binds = b
		default:
			return nil, c.error("Fetch's 2nd param (binds) must be []interface{}")
		}
	}
	var schema string
	if len(args) > 1 && args[1] != nil {
		switch s := args[1].(type) {
		case string:
			schema = s
		default:
			return nil, c.error("Fetch's 3nd param (schema) must be a string")
		}
	}

	resp, err := c.execute(sql, [][]interface{}{binds}, schema, nil, false)
	if err != nil {
		return nil, c.errorf("Unable to Fetch: %s", err)
	}
	respData := resp.ResponseData
	if respData.NumResults != 1 {
		return nil, c.errorf("Unexpected numResults: %v", respData.NumResults)
	}
	result := respData.Results[0]
	if result.ResultType != resultSetType {
		return nil, c.errorf("Unexpected result type: %v", result.ResultType)
	}
	if result.ResultSet == nil {
		return nil, c.error("Missing websocket API resultset")
	}

	ch := make(chan FetchResult, 1000)
	go c.resultsToChan(result.ResultSet, ch)

	return ch, nil
}

// For large datasets use FetchChan to avoid buffering all the data in memory
func (c *Conn) FetchSlice(sql string, args ...interface{}) (res [][]interface{}, err error) {
	resChan, err := c.FetchChan(sql, args...)
	if err != nil {
		return nil, err
	}
	for row := range resChan {
		res = append(res, row.Data)
	}
	return res, nil
}

func (c *Conn) SetTimeout(timeout uint32) error {
	err := c.send(&request{
		Command:    "setAttributes",
		Attributes: &Attributes{QueryTimeout: timeout},
	}, &response{})
	if err != nil {
		return c.errorf("Unable to set timeout: %s", err)
	}
	return nil
}

// Gets a sync.Mutext lock on the handle.
// Allows coordinating use of the handle across multiple Go routines
func (c *Conn) Lock()   { c.mux.Lock() }
func (c *Conn) Unlock() { c.mux.Unlock() }

/*--- Private Routines ---*/

func (c *Conn) login() error {
	loginReq := &loginReq{
		Command:         "login",
		ProtocolVersion: ExasolAPIVersion,
	}
	loginRes := &loginRes{}
	err := c.send(loginReq, loginRes)
	if err != nil {
		return err
	}

	pubKeyMod, _ := hex.DecodeString(loginRes.ResponseData.PublicKeyModulus)
	var modulus big.Int
	modulus.SetBytes(pubKeyMod)

	pubKeyExp, _ := strconv.ParseUint(loginRes.ResponseData.PublicKeyExponent, 16, 32)

	pubKey := rsa.PublicKey{
		N: &modulus,
		E: int(pubKeyExp),
	}
	password := []byte(c.Conf.Password)
	encPass, err := rsa.EncryptPKCS1v15(rand.Reader, &pubKey, password)
	if err != nil {
		return fmt.Errorf("Password encryption error: %s", err)
	}
	b64Pass := base64.StdEncoding.EncodeToString(encPass)

	osUser, _ := user.Current()

	authReq := &authReq{
		Username:         c.Conf.Username,
		Password:         b64Pass,
		UseCompression:   false, // TODO: See if we can get compression working
		ClientName:       c.Conf.ClientName,
		ClientVersion:    c.Conf.ClientVersion, // The version of the calling application
		DriverName:       "go-exasol-client v" + DriverVersion,
		ClientOs:         runtime.GOOS,
		ClientOsUsername: osUser.Username,
		ClientRuntime:    runtime.Version(),
		Attributes:       &Attributes{Autocommit: true}, // Default AutoCommit to on
	}

	if c.Conf.QueryTimeout.Seconds() > 0 {
		authReq.Attributes.QueryTimeout = uint32(c.Conf.QueryTimeout.Seconds())
	}

	authResp := &authResp{}
	err = c.send(authReq, authResp)
	if err != nil {
		return fmt.Errorf("Unable to authenticate: %s", err)
	}

	c.SessionID = authResp.ResponseData.SessionID
	c.Metadata = authResp.ResponseData
	c.log.Info("Connected SessionID:", c.SessionID)
	c.wsh.EnableCompression(false)

	return nil
}

func (c *Conn) execute(
	sql string,
	binds [][]interface{},
	schema string,
	dataTypes []DataType,
	isColumnar bool,
) (*execRes, error) {
	// Just a simple execute (no prepare) if there are no binds
	if binds == nil || len(binds) == 0 ||
		binds[0] == nil || len(binds[0]) == 0 {
		c.log.Debug("Execute: ", sql)
		req := &execReq{
			Command:    "execute",
			Attributes: &Attributes{CurrentSchema: schema},
			SqlText:    sql,
		}
		res := &execRes{}
		err := c.send(req, res)
		return res, err
	} else {
		return c.executePrepStmt(sql, binds, schema, dataTypes, isColumnar)
	}
}

func (c *Conn) executePrepStmt(
	sql string,
	binds [][]interface{},
	schema string,
	dataTypes []DataType,
	isColumnar bool,
) (*execRes, error) {
	// There are binds so we need to send data so do a prepare + execute
	ps, err := c.getPrepStmt(schema, sql)
	if err != nil {
		return nil, err
	}

	// This is to workaround this bug: https://www.exasol.com/support/browse/EXASOL-2138
	if dataTypes != nil {
		for i, dt := range dataTypes {
			ps.columns[i].DataType = dt
		}
	}

	if !isColumnar {
		binds = Transpose(binds)
	}
	numCols := len(binds)
	numRows := len(binds[0])

	c.log.Debugf("Executing %d x %d stmt", numCols, numRows)
	req := &execPrepStmt{
		Command:         "executePreparedStatement",
		StatementHandle: int(ps.sth),
		NumColumns:      numCols,
		NumRows:         numRows,
		Columns:         ps.columns,
		Data:            binds,
	}
	res := &execRes{}
	err = c.send(req, res)

	if err != nil &&
		regexp.MustCompile("Statement handle not found").MatchString(err.Error()) {
		// Not sure what causes this but I've seen it happen. So just try again.
		c.log.Warning("Statement handle not found:", ps.sth)
		delete(c.prepStmtCache, sql)
		ps, err := c.getPrepStmt(schema, sql)
		if err != nil {
			return nil, err
		}
		c.log.Warning("Retrying with:", ps.sth)
		req.StatementHandle = int(ps.sth)
		err = c.send(req, res)
	}
	if !c.Conf.CachePrepStmts {
		c.closePrepStmt(ps.sth)
	}
	return res, err
}

func (c *Conn) resultsToChan(rs *resultSet, ch chan<- FetchResult) {
	defer func() {
		close(ch)
	}()

	if rs.NumRows == 0 {
		// Do nothing
	} else if rs.ResultSetHandle > 0 {
		for i := uint64(0); i < rs.NumRows; {
			fetchReq := &fetchReq{
				Command:         "fetch",
				ResultSetHandle: rs.ResultSetHandle,
				StartPosition:   i,
				NumBytes:        c.Conf.FetchReqSize,
			}
			fetchRes := &fetchRes{}
			err := c.send(fetchReq, fetchRes)
			if err != nil {
				ch <- FetchResult{Error: err}
				return
			}
			i += fetchRes.ResponseData.NumRows
			err = transposeToChan(c.ctx, ch, fetchRes.ResponseData.Data)
			if err != nil {
				ch <- FetchResult{
					Error: err,
				}
				c.log.Warning("Error send to result channel:", err)
				return
			}
		}

		closeRSReq := &closeResultSet{
			Command:          "closeResultSet",
			ResultSetHandles: []int{rs.ResultSetHandle},
		}
		err := c.send(closeRSReq, &response{})
		if err != nil {
			c.log.Warning("Unable to close result set:", err)
		}
	} else {
		err := transposeToChan(c.ctx, ch, rs.Data)
		if err != nil {
			ch <- FetchResult{
				Error: err,
			}
			c.log.Warning("Error send to result channel:", err)
			return
		}
	}
}
