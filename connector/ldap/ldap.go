// Package ldap implements strategies for authenticating using the LDAP protocol.
package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"

	"gopkg.in/ldap.v2"

	"github.com/sirupsen/logrus"

	"github.com/concourse/dex/connector"
)

// Config holds the configuration parameters for the LDAP connector. The LDAP
// connectors require executing two queries, the first to find the user based on
// the username and password given to the connector. The second to use the user
// entry to search for groups.
//
// An example config:
//
//     type: ldap
//     config:
//       host: ldap.example.com:636
//       # The following field is required if using port 389.
//       # insecureNoSSL: true
//       rootCA: /etc/dex/ldap.ca
//       bindDN: uid=seviceaccount,cn=users,dc=example,dc=com
//       bindPW: password
//       userSearch:
//         # Would translate to the query "(&(objectClass=person)(uid=<username>))"
//         baseDN: cn=users,dc=example,dc=com
//         filter: "(objectClass=person)"
//         username: uid
//         idAttr: uid
//         emailAttr: mail
//         nameAttr: name
//       groupSearch:
//         # Would translate to the query "(&(objectClass=group)(member=<user uid>))"
//         baseDN: cn=groups,dc=example,dc=com
//         filter: "(objectClass=group)"
//         userAttr: uid
//         # Use if full DN is needed and not available as any other attribute
//         # Will only work if "DN" attribute does not exist in the record
//         # userAttr: DN
//         groupAttr: member
//         nameAttr: name
//
type Config struct {
	// The host and optional port of the LDAP server. If port isn't supplied, it will be
	// guessed based on the TLS configuration. 389 or 636.
	Host string `json:"host"`

	// Required if LDAP host does not use TLS.
	InsecureNoSSL bool `json:"insecureNoSSL"`

	// Don't verify the CA.
	InsecureSkipVerify bool `json:"insecureSkipVerify"`

	// Connect to the insecure port then issue a StartTLS command to negotiate a
	// secure connection. If unsupplied secure connections will use the LDAPS
	// protocol.
	StartTLS bool `json:"startTLS"`

	// Path to a trusted root certificate file.
	RootCA string `json:"rootCA"`

	// Base64 encoded PEM data containing root CAs.
	RootCAData []byte `json:"rootCAData"`

	// BindDN and BindPW for an application service account. The connector uses these
	// credentials to search for users and groups.
	BindDN string `json:"bindDN"`
	BindPW string `json:"bindPW"`

	// UsernamePrompt allows users to override the username attribute (displayed
	// in the username/password prompt). If unset, the handler will use
	// "Username".
	UsernamePrompt string `json:"usernamePrompt"`

	// User entry search configuration.
	UserSearch struct {
		// BaseDN to start the search from. For example "cn=users,dc=example,dc=com"
		BaseDN string `json:"baseDN"`

		// Optional filter to apply when searching the directory. For example "(objectClass=person)"
		Filter string `json:"filter"`

		// Attribute to match against the inputted username. This will be translated and combined
		// with the other filter as "(<attr>=<username>)".
		Username string `json:"username"`

		// Can either be:
		// * "sub" - search the whole sub tree
		// * "one" - only search one level
		Scope string `json:"scope"`

		// A mapping of attributes on the user entry to claims.
		IDAttr    string `json:"idAttr"`    // Defaults to "uid"
		EmailAttr string `json:"emailAttr"` // Defaults to "mail"
		NameAttr  string `json:"nameAttr"`  // No default.

	} `json:"userSearch"`

	// Group search configuration.
	GroupSearch struct {
		// BaseDN to start the search from. For example "cn=groups,dc=example,dc=com"
		BaseDN string `json:"baseDN"`

		// Optional filter to apply when searching the directory. For example "(objectClass=posixGroup)"
		Filter string `json:"filter"`

		Scope string `json:"scope"` // Defaults to "sub"

		// These two fields are use to match a user to a group.
		//
		// It adds an additional requirement to the filter that an attribute in the group
		// match the user's attribute value. For example that the "members" attribute of
		// a group matches the "uid" of the user. The exact filter being added is:
		//
		//   (<groupAttr>=<userAttr value>)
		//
		UserAttr  string `json:"userAttr"`
		GroupAttr string `json:"groupAttr"`

		// The attribute of the group that represents its name.
		NameAttr string `json:"nameAttr"`
	} `json:"groupSearch"`
}

func scopeString(i int) string {
	switch i {
	case ldap.ScopeBaseObject:
		return "base"
	case ldap.ScopeSingleLevel:
		return "one"
	case ldap.ScopeWholeSubtree:
		return "sub"
	default:
		return ""
	}
}

func parseScope(s string) (int, bool) {
	// NOTE(ericchiang): ScopeBaseObject doesn't really make sense for us because we
	// never know the user's or group's DN.
	switch s {
	case "", "sub":
		return ldap.ScopeWholeSubtree, true
	case "one":
		return ldap.ScopeSingleLevel, true
	}
	return 0, false
}

// Open returns an authentication strategy using LDAP.
func (c *Config) Open(id string, logger logrus.FieldLogger) (connector.Connector, error) {
	conn, err := c.OpenConnector(logger)
	if err != nil {
		return nil, err
	}
	return connector.Connector(conn), nil
}

type refreshData struct {
	Username string     `json:"username"`
	Entry    ldap.Entry `json:"entry"`
}

// OpenConnector is the same as Open but returns a type with all implemented connector interfaces.
func (c *Config) OpenConnector(logger logrus.FieldLogger) (interface {
	connector.Connector
	connector.PasswordConnector
	connector.RefreshConnector
}, error) {
	return c.openConnector(logger)
}

func (c *Config) openConnector(logger logrus.FieldLogger) (*ldapConnector, error) {

	requiredFields := []struct {
		name string
		val  string
	}{
		{"host", c.Host},
		{"userSearch.baseDN", c.UserSearch.BaseDN},
		{"userSearch.username", c.UserSearch.Username},
	}

	for _, field := range requiredFields {
		if field.val == "" {
			return nil, fmt.Errorf("ldap: missing required field %q", field.name)
		}
	}

	var (
		host string
		err  error
	)
	if host, _, err = net.SplitHostPort(c.Host); err != nil {
		host = c.Host
		if c.InsecureNoSSL {
			c.Host = c.Host + ":389"
		} else {
			c.Host = c.Host + ":636"
		}
	}

	tlsConfig := &tls.Config{ServerName: host, InsecureSkipVerify: c.InsecureSkipVerify}
	if c.RootCA != "" || len(c.RootCAData) != 0 {
		data := c.RootCAData
		if len(data) == 0 {
			var err error
			if data, err = ioutil.ReadFile(c.RootCA); err != nil {
				return nil, fmt.Errorf("ldap: read ca file: %v", err)
			}
		}
		rootCAs := x509.NewCertPool()
		if !rootCAs.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("ldap: no certs found in ca file")
		}
		tlsConfig.RootCAs = rootCAs
	}
	userSearchScope, ok := parseScope(c.UserSearch.Scope)
	if !ok {
		return nil, fmt.Errorf("userSearch.Scope unknown value %q", c.UserSearch.Scope)
	}
	groupSearchScope, ok := parseScope(c.GroupSearch.Scope)
	if !ok {
		return nil, fmt.Errorf("groupSearch.Scope unknown value %q", c.GroupSearch.Scope)
	}
	return &ldapConnector{*c, userSearchScope, groupSearchScope, tlsConfig, logger}, nil
}

type ldapConnector struct {
	Config

	userSearchScope  int
	groupSearchScope int

	tlsConfig *tls.Config

	logger logrus.FieldLogger
}

var (
	_ connector.PasswordConnector = (*ldapConnector)(nil)
	_ connector.RefreshConnector  = (*ldapConnector)(nil)
)

// do initializes a connection to the LDAP directory and passes it to the
// provided function. It then performs appropriate teardown or reuse before
// returning.
func (c *ldapConnector) do(ctx context.Context, f func(c *ldap.Conn) error) error {
	// TODO(ericchiang): support context here
	var (
		conn *ldap.Conn
		err  error
	)
	switch {
	case c.InsecureNoSSL:
		conn, err = ldap.Dial("tcp", c.Host)
	case c.StartTLS:
		conn, err = ldap.Dial("tcp", c.Host)
		if err != nil {
			return fmt.Errorf("failed to connect: %v", err)
		}
		if err := conn.StartTLS(c.tlsConfig); err != nil {
			return fmt.Errorf("start TLS failed: %v", err)
		}
	default:
		conn, err = ldap.DialTLS("tcp", c.Host, c.tlsConfig)
	}
	if err != nil {
		return fmt.Errorf("failed to connect: %v", err)
	}
	defer conn.Close()

	// If bindDN and bindPW are empty this will default to an anonymous bind.
	if err := conn.Bind(c.BindDN, c.BindPW); err != nil {
		return fmt.Errorf("ldap: initial bind for user %q failed: %v", c.BindDN, err)
	}

	return f(conn)
}

func getAttrs(e ldap.Entry, name string) []string {
	for _, a := range e.Attributes {
		if a.Name != name {
			continue
		}
		return a.Values
	}
	if name == "DN" {
		return []string{e.DN}
	}
	return nil
}

func getAttr(e ldap.Entry, name string) string {
	if a := getAttrs(e, name); len(a) > 0 {
		return a[0]
	}
	return ""
}

func (c *ldapConnector) identityFromEntry(user ldap.Entry) (ident connector.Identity, err error) {
	// If we're missing any attributes, such as email or ID, we want to report
	// an error rather than continuing.
	missing := []string{}

	// Fill the identity struct using the attributes from the user entry.
	if ident.UserID = getAttr(user, c.UserSearch.IDAttr); ident.UserID == "" {
		missing = append(missing, c.UserSearch.IDAttr)
	}
	if ident.Email = getAttr(user, c.UserSearch.EmailAttr); ident.Email == "" {
		missing = append(missing, c.UserSearch.EmailAttr)
	}
	// TODO(ericchiang): Let this value be set from an attribute.
	ident.EmailVerified = true

	if c.UserSearch.Username != "" {
		if ident.Username = getAttr(user, c.UserSearch.Username); ident.Username == "" {
			missing = append(missing, c.UserSearch.Username)
		}
	}

	if c.UserSearch.NameAttr != "" {
		if ident.Name = getAttr(user, c.UserSearch.NameAttr); ident.Name == "" {
			missing = append(missing, c.UserSearch.NameAttr)
		}
	}

	if len(missing) != 0 {
		err := fmt.Errorf("ldap: entry %q missing following required attribute(s): %q", user.DN, missing)
		return connector.Identity{}, err
	}
	return ident, nil
}

func (c *ldapConnector) userEntry(conn *ldap.Conn, username string) (user ldap.Entry, found bool, err error) {

	filter := fmt.Sprintf("(%s=%s)", c.UserSearch.Username, ldap.EscapeFilter(username))
	if c.UserSearch.Filter != "" {
		filter = fmt.Sprintf("(&%s%s)", c.UserSearch.Filter, filter)
	}

	// Initial search.
	req := &ldap.SearchRequest{
		BaseDN: c.UserSearch.BaseDN,
		Filter: filter,
		Scope:  c.userSearchScope,
		// We only need to search for these specific requests.
		Attributes: []string{
			c.UserSearch.IDAttr,
			c.UserSearch.EmailAttr,
			c.GroupSearch.UserAttr,
			// TODO(ericchiang): what if this contains duplicate values?
		},
	}

	if c.UserSearch.NameAttr != "" {
		req.Attributes = append(req.Attributes, c.UserSearch.NameAttr)
	}

	c.logger.Infof("performing ldap search %s %s %s",
		req.BaseDN, scopeString(req.Scope), req.Filter)
	resp, err := conn.Search(req)
	if err != nil {
		return ldap.Entry{}, false, fmt.Errorf("ldap: search with filter %q failed: %v", req.Filter, err)
	}

	switch n := len(resp.Entries); n {
	case 0:
		c.logger.Errorf("ldap: no results returned for filter: %q", filter)
		return ldap.Entry{}, false, nil
	case 1:
		user = *resp.Entries[0]
		c.logger.Infof("username %q mapped to entry %s", username, user.DN)
		return user, true, nil
	default:
		return ldap.Entry{}, false, fmt.Errorf("ldap: filter returned multiple (%d) results: %q", n, filter)
	}
}

func (c *ldapConnector) Login(ctx context.Context, s connector.Scopes, username, password string) (ident connector.Identity, validPass bool, err error) {
	// make this check to avoid unauthenticated bind to the LDAP server.
	if password == "" {
		return connector.Identity{}, false, nil
	}

	var (
		// We want to return a different error if the user's password is incorrect vs
		// if there was an error.
		incorrectPass = false
		user          ldap.Entry
	)

	err = c.do(ctx, func(conn *ldap.Conn) error {
		entry, found, err := c.userEntry(conn, username)
		if err != nil {
			return err
		}
		if !found {
			incorrectPass = true
			return nil
		}
		user = entry

		// Try to authenticate as the distinguished name.
		if err := conn.Bind(user.DN, password); err != nil {
			// Detect a bad password through the LDAP error code.
			if ldapErr, ok := err.(*ldap.Error); ok {
				switch ldapErr.ResultCode {
				case ldap.LDAPResultInvalidCredentials:
					c.logger.Errorf("ldap: invalid password for user %q", user.DN)
					incorrectPass = true
					return nil
				case ldap.LDAPResultConstraintViolation:
					c.logger.Errorf("ldap: constraint violation for user %q: %s", user.DN, ldapErr.Error())
					incorrectPass = true
					return nil
				}
			} // will also catch all ldap.Error without a case statement above
			return fmt.Errorf("ldap: failed to bind as dn %q: %v", user.DN, err)
		}
		return nil
	})
	if err != nil {
		return connector.Identity{}, false, err
	}
	if incorrectPass {
		return connector.Identity{}, false, nil
	}

	if ident, err = c.identityFromEntry(user); err != nil {
		return connector.Identity{}, false, err
	}

	if s.Groups {
		groups, err := c.groups(ctx, user)
		if err != nil {
			return connector.Identity{}, false, fmt.Errorf("ldap: failed to query groups: %v", err)
		}
		ident.Groups = groups
	}

	if s.OfflineAccess {
		refresh := refreshData{
			Username: username,
			Entry:    user,
		}
		// Encode entry for follow up requests such as the groups query and
		// refresh attempts.
		if ident.ConnectorData, err = json.Marshal(refresh); err != nil {
			return connector.Identity{}, false, fmt.Errorf("ldap: marshal entry: %v", err)
		}
	}

	return ident, true, nil
}

func (c *ldapConnector) Refresh(ctx context.Context, s connector.Scopes, ident connector.Identity) (connector.Identity, error) {
	var data refreshData
	if err := json.Unmarshal(ident.ConnectorData, &data); err != nil {
		return ident, fmt.Errorf("ldap: failed to unamrshal internal data: %v", err)
	}

	var user ldap.Entry
	err := c.do(ctx, func(conn *ldap.Conn) error {
		entry, found, err := c.userEntry(conn, data.Username)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("ldap: user not found %q", data.Username)
		}
		user = entry
		return nil
	})
	if err != nil {
		return ident, err
	}
	if user.DN != data.Entry.DN {
		return ident, fmt.Errorf("ldap: refresh for username %q expected DN %q got %q", data.Username, data.Entry.DN, user.DN)
	}

	newIdent, err := c.identityFromEntry(user)
	if err != nil {
		return ident, err
	}
	newIdent.ConnectorData = ident.ConnectorData

	if s.Groups {
		groups, err := c.groups(ctx, user)
		if err != nil {
			return connector.Identity{}, fmt.Errorf("ldap: failed to query groups: %v", err)
		}
		newIdent.Groups = groups
	}
	return newIdent, nil
}

func (c *ldapConnector) groups(ctx context.Context, user ldap.Entry) ([]string, error) {
	if c.GroupSearch.BaseDN == "" {
		c.logger.Debugf("No groups returned for %q because no groups baseDN has been configured.", getAttr(user, c.UserSearch.NameAttr))
		return nil, nil
	}

	var groups []*ldap.Entry
	for _, attr := range getAttrs(user, c.GroupSearch.UserAttr) {
		filter := fmt.Sprintf("(%s=%s)", c.GroupSearch.GroupAttr, ldap.EscapeFilter(attr))
		if c.GroupSearch.Filter != "" {
			filter = fmt.Sprintf("(&%s%s)", c.GroupSearch.Filter, filter)
		}

		req := &ldap.SearchRequest{
			BaseDN:     c.GroupSearch.BaseDN,
			Filter:     filter,
			Scope:      c.groupSearchScope,
			Attributes: []string{c.GroupSearch.NameAttr},
		}

		gotGroups := false
		if err := c.do(ctx, func(conn *ldap.Conn) error {
			c.logger.Infof("performing ldap search %s %s %s",
				req.BaseDN, scopeString(req.Scope), req.Filter)
			resp, err := conn.Search(req)
			if err != nil {
				return fmt.Errorf("ldap: search failed: %v", err)
			}
			gotGroups = len(resp.Entries) != 0
			groups = append(groups, resp.Entries...)
			return nil
		}); err != nil {
			return nil, err
		}
		if !gotGroups {
			// TODO(ericchiang): Is this going to spam the logs?
			c.logger.Errorf("ldap: groups search with filter %q returned no groups", filter)
		}
	}

	var groupNames []string
	for _, group := range groups {
		name := getAttr(*group, c.GroupSearch.NameAttr)
		if name == "" {
			// Be obnoxious about missing missing attributes. If the group entry is
			// missing its name attribute, that indicates a misconfiguration.
			//
			// In the future we can add configuration options to just log these errors.
			return nil, fmt.Errorf("ldap: group entity %q missing required attribute %q",
				group.DN, c.GroupSearch.NameAttr)
		}

		groupNames = append(groupNames, name)
	}
	return groupNames, nil
}

func (c *ldapConnector) Prompt() string {
	return c.UsernamePrompt
}
