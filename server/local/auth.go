package local

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/dgrijalva/jwt-go"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"sourcegraph.com/sourcegraph/go-sourcegraph/sourcegraph"
	"sourcegraph.com/sqs/pbtypes"
	"src.sourcegraph.com/sourcegraph/app/router"
	authpkg "src.sourcegraph.com/sourcegraph/auth"
	"src.sourcegraph.com/sourcegraph/auth/accesstoken"
	"src.sourcegraph.com/sourcegraph/auth/authutil"
	"src.sourcegraph.com/sourcegraph/auth/idkey"
	"src.sourcegraph.com/sourcegraph/auth/ldap"
	"src.sourcegraph.com/sourcegraph/conf"
	"src.sourcegraph.com/sourcegraph/fed"
	"src.sourcegraph.com/sourcegraph/pkg/oauth2util"
	"src.sourcegraph.com/sourcegraph/store"
	"src.sourcegraph.com/sourcegraph/svc"
	"src.sourcegraph.com/sourcegraph/util/randstring"
)

var Auth sourcegraph.AuthServer = &auth{}

type auth struct{}

var _ sourcegraph.AuthServer = (*auth)(nil)

func (s *auth) GetAuthorizationCode(ctx context.Context, op *sourcegraph.AuthorizationCodeRequest) (*sourcegraph.AuthorizationCode, error) {
	authStore := store.AuthorizationsFromContextOrNil(ctx)
	if authStore == nil {
		return nil, grpc.Errorf(codes.Unimplemented, "no Authorizations")
	}

	if op.ResponseType != "code" {
		return nil, grpc.Errorf(codes.InvalidArgument, "invalid response_type")
	}

	client, err := (&registeredClients{}).Get(ctx, &sourcegraph.RegisteredClientSpec{ID: op.ClientID})
	if err != nil {
		return nil, err
	}

	if uid := authpkg.ActorFromContext(ctx).UID; uid == 0 || uid != int(op.UID) {
		return nil, grpc.Errorf(codes.PermissionDenied, "user %d attempted to create auth code for user %d", uid, op.UID)
	}

	ctx2 := authpkg.WithActor(ctx, authpkg.Actor{UID: int(op.UID), ClientID: op.ClientID})
	if userPerms, err := s.GetPermissions(ctx2, &pbtypes.Void{}); err != nil {
		return nil, err
	} else if !(userPerms.Read || userPerms.Write || userPerms.Admin) {
		return nil, grpc.Errorf(codes.PermissionDenied, "user %d is not allowed to log into this server", op.UID)
	}

	// RedirectURI is OPTIONAL
	// (https://tools.ietf.org/html/rfc6749#section-4.1.1) but must be
	// validated if set.
	if op.RedirectURI != "" {
		if err := oauth2util.AllowRedirectURI(client.RedirectURIs, op.RedirectURI); err != nil {
			return nil, err
		}
	}

	code, err := authStore.CreateAuthCode(ctx, op, 5*time.Minute)
	if err != nil {
		return nil, err
	}
	return &sourcegraph.AuthorizationCode{Code: code, RedirectURI: op.RedirectURI}, nil
}

func (s *auth) GetAccessToken(ctx context.Context, op *sourcegraph.AccessTokenRequest) (*sourcegraph.AccessTokenResponse, error) {
	if authCode := op.GetAuthorizationCode(); authCode != nil {
		return s.exchangeCodeForAccessToken(ctx, authCode)
	} else if resOwnerPassword := op.GetResourceOwnerPassword(); resOwnerPassword != nil {
		return s.authenticateLogin(ctx, resOwnerPassword)
	} else if bearerJWT := op.GetBearerJWT(); bearerJWT != nil {
		return s.authenticateBearerJWT(ctx, bearerJWT)
	} else {
		return nil, grpc.Errorf(codes.Unauthenticated, "no supported auth credentials provided")
	}
}

func (s *auth) exchangeCodeForAccessToken(ctx context.Context, code *sourcegraph.AuthorizationCode) (*sourcegraph.AccessTokenResponse, error) {
	authStore := store.AuthorizationsFromContextOrNil(ctx)
	if authStore == nil {
		return nil, grpc.Errorf(codes.Unimplemented, "no Authorizations")
	}

	usersStore := store.UsersFromContextOrNil(ctx)
	if usersStore == nil {
		return nil, grpc.Errorf(codes.Unimplemented, "no Users")
	}

	clientID := authpkg.ActorFromContext(ctx).ClientID
	client, err := (&registeredClients{}).Get(ctx, &sourcegraph.RegisteredClientSpec{ID: clientID})
	if err != nil {
		return nil, err
	}

	// RedirectURI is REQUIRED if one was provided when the code was
	// created (https://tools.ietf.org/html/rfc6749#section-4.1.3).
	if code.RedirectURI != "" {
		if err := oauth2util.AllowRedirectURI(client.RedirectURIs, code.RedirectURI); err != nil {
			return nil, err
		}
	}

	req, err := authStore.MarkExchanged(ctx, code, authpkg.ActorFromContext(ctx).ClientID)
	if err != nil {
		return nil, err
	}

	user, err := usersStore.Get(ctx, sourcegraph.UserSpec{UID: req.UID})
	if err != nil {
		return nil, err
	}

	tok, err := accesstoken.New(idkey.FromContext(ctx), authpkg.Actor{
		UID:      int(user.UID),
		Login:    user.Login,
		ClientID: req.ClientID,
		Scope:    req.Scope,
	}, map[string]string{"GrantType": "AuthorizationCode"}, 7*24*time.Hour)
	if err != nil {
		return nil, err
	}

	return accessTokenToTokenResponse(tok), nil
}

func (s *auth) authenticateLogin(ctx context.Context, cred *sourcegraph.LoginCredentials) (*sourcegraph.AccessTokenResponse, error) {
	usersStore := store.UsersFromContextOrNil(ctx)
	if usersStore == nil {
		return nil, grpc.Errorf(codes.Unimplemented, "no Users")
	}

	user, err := usersStore.Get(ctx, sourcegraph.UserSpec{Login: cred.Login})
	if err != nil {
		if !(store.IsUserNotFound(err) && authutil.ActiveFlags.IsLDAP()) {
			return nil, err
		}
	}

	if authutil.ActiveFlags.IsLDAP() {
		ldapuser, err := ldap.VerifyLogin(cred.Login, cred.Password)
		if err != nil {
			return nil, grpc.Errorf(codes.PermissionDenied, "LDAP auth failed: %v", err)
		}

		if user == nil {
			user, err = linkLDAPUserAccount(ctx, ldapuser)
			if err != nil {
				return nil, err
			}
		}

		// TODO(pararth): make federated user permissions work for LDAP instances.
		// Verify login permissions via federation root.
		// ctx2 := authpkg.WithActor(ctx, authpkg.Actor{UID: int(user.UID), ClientID: idkey.FromContext(ctx).ID})
		// if userPerms, err := svc.Auth(ctx2).GetPermissions(ctx2, &pbtypes.Void{}); err != nil {
		// 	return nil, err
		// } else if !(userPerms.Read || userPerms.Write || userPerms.Admin) {
		// 	return nil, grpc.Errorf(codes.PermissionDenied, "User %v (UID %v) is not allowed to log in", ldapuser.Username, user.UID)
		// }
	} else {
		passwordStore := store.PasswordFromContextOrNil(ctx)
		if passwordStore == nil {
			return nil, grpc.Errorf(codes.Unimplemented, "no Passwords")
		}

		if passwordStore.CheckUIDPassword(ctx, user.UID, cred.Password) != nil {
			return nil, grpc.Errorf(codes.PermissionDenied, "bad password for user %q", cred.Login)
		}
	}

	a := authpkg.ActorFromContext(ctx)

	// TODO(public-release): This is commented out to allow the gitserver to fetch access
	// tokens directly via the gRPC endpoint of the federation root. This is non-standard
	// OAuth2 API usage, and must be changed to fetching the tokens over HTTP, so that this
	// check can be added back in.
	// k := idkey.FromContext(ctx)
	// if k.ID != a.ClientID {
	// 	return nil, grpc.Errorf(codes.Unauthenticated, "client authentication as %q (actor client ID is %q) required to access token from resource owner password (user %q)", k.ID, a.ClientID, cred.Login)
	// }
	if a.IsUser() {
		return nil, grpc.Errorf(codes.PermissionDenied, "refusing to issue access token from resource owner password to already authenticated user %d (only client, not user, must be authenticated)", a.UID)
	}

	tok, err := accesstoken.New(idkey.FromContext(ctx), authpkg.Actor{
		UID:      int(user.UID),
		Login:    user.Login,
		ClientID: a.ClientID,
	}, map[string]string{"GrantType": "ResourceOwnerPassword"}, 7*24*time.Hour)
	if err != nil {
		return nil, err
	}

	return accessTokenToTokenResponse(tok), nil
}

func (s *auth) authenticateBearerJWT(ctx context.Context, rawTok *sourcegraph.BearerJWT) (*sourcegraph.AccessTokenResponse, error) {
	var regClient *sourcegraph.RegisteredClient
	tok, err := jwt.Parse(rawTok.Assertion, func(tok *jwt.Token) (interface{}, error) {
		// The JWT's "iss" is the client's OAuth2 client ID.
		clientID, _ := tok.Claims["iss"].(string)
		if clientID == "" {
			return nil, errors.New("bearer JWT has empty issuer, can't look up key")
		}
		var err error
		regClient, err = (&registeredClients{}).Get(ctx, &sourcegraph.RegisteredClientSpec{ID: clientID})
		if err != nil {
			return nil, err
		}

		// Get the client's registered public key.
		if regClient.JWKS == "" {
			return nil, fmt.Errorf("client ID %s (identified by bearer JWT) has no JWKS", clientID)
		}
		pubKey, err := idkey.UnmarshalJWKSPublicKey([]byte(regClient.JWKS))
		if err != nil {
			return nil, fmt.Errorf("parsing client ID %s JWKS public key: %s", clientID, err)
		}
		return pubKey, nil
	})
	if err != nil {
		return nil, err
	}

	// Validate claims; see
	// https://tools.ietf.org/html/draft-ietf-oauth-jwt-bearer-12#section-3.
	aud, _ := tok.Claims["aud"].(string)
	tokURL := conf.AppURL(ctx).ResolveReference(router.Rel.URLTo(router.OAuth2ServerToken))
	if subtle.ConstantTimeCompare([]byte(aud), []byte(tokURL.String())) != 1 {
		return nil, grpc.Errorf(codes.PermissionDenied, "bearer JWT aud claim mismatch (JWT %q, server %q)", aud, tokURL)
	}

	atok, err := accesstoken.New(
		idkey.FromContext(ctx),
		authpkg.Actor{ClientID: regClient.ID},
		map[string]string{"GrantType": "BearerJWT"},
		time.Hour,
	)
	if err != nil {
		return nil, err
	}

	return accessTokenToTokenResponse(atok), nil
}

func accessTokenToTokenResponse(t *oauth2.Token) *sourcegraph.AccessTokenResponse {
	if t.AccessToken == "" {
		panic("empty AccessToken")
	}
	if t.TokenType == "" {
		panic("empty TokenType")
	}
	r := &sourcegraph.AccessTokenResponse{
		AccessToken: t.AccessToken,
		TokenType:   t.TokenType,
	}
	if !t.Expiry.IsZero() {
		sec := t.Expiry.Sub(time.Now()) / time.Second
		if sec > math.MaxInt32 {
			sec = math.MaxInt32
		}
		r.ExpiresInSec = int32(sec)
	}
	return r
}

func (s *auth) Identify(ctx context.Context, _ *pbtypes.Void) (*sourcegraph.AuthInfo, error) {
	// TODO(sqs): cache until the expiration of the token
	shortCache(ctx)

	a := authpkg.ActorFromContext(ctx)
	return &sourcegraph.AuthInfo{
		ClientID: a.ClientID,
		UID:      int32(a.UID),
		Login:    a.Login,
		Domain:   a.Domain,
	}, nil
}

func (s *auth) GetPermissions(ctx context.Context, _ *pbtypes.Void) (*sourcegraph.UserPermissions, error) {
	a := authpkg.ActorFromContext(ctx)
	if a.UID == 0 || a.ClientID == "" {
		return nil, grpc.Errorf(codes.Unauthenticated, "no authenticated actor or client in context")
	}

	userPerms := &sourcegraph.UserPermissions{UID: int32(a.UID), ClientID: a.ClientID}

	// set this flag for testing only; never to be set in production on root server.
	if authutil.ActiveFlags.AllowAllLogins {
		userPerms.Read = true
		userPerms.Write = true
		return userPerms, nil
	}

	if a.ClientID == idkey.FromContext(ctx).ID {
		// Fetch permissions for a user account local to this server.
		user, err := svc.Users(ctx).Get(ctx, &sourcegraph.UserSpec{UID: int32(a.UID)})
		if err != nil && grpc.Code(err) == codes.NotFound {
			return userPerms, nil
		} else if err != nil {
			return nil, err
		}
		userPerms.Read = true
		userPerms.Write = true
		userPerms.Admin = user.Admin
		return userPerms, nil
	}

	// check user's access permissions for the client.
	userPermsOpt := &sourcegraph.UserPermissionsOptions{
		UID:        int32(a.UID),
		ClientSpec: &sourcegraph.RegisteredClientSpec{ID: a.ClientID},
	}

	var err error
	userPerms, err = svc.RegisteredClients(ctx).GetUserPermissions(ctx, userPermsOpt)
	if err != nil {
		// ignore NotImplementedError as the UserPermissionsStore is not implemented
		if _, ok := err.(*sourcegraph.NotImplementedError); !ok {
			return nil, err
		}
	}

	if !(userPerms.Read || userPerms.Write || userPerms.Admin) {
		// check if the client allows all logins.
		client, err := svc.RegisteredClients(ctx).Get(ctx, &sourcegraph.RegisteredClientSpec{ID: a.ClientID})
		if err != nil {
			return nil, err
		}
		if client.Meta != nil && client.Meta["allow-logins"] == "all" {
			// activate this user on the client.
			userPerms.Read = true
			if client.Meta["default-access"] == "write" {
				userPerms.Write = true
			}
			// WARN(security): using the non-strict setPermissionsForUser method which doesn't
			// enforce perms checks on the user setting the permissions since the current user
			// is not an admin on the client. Be careful about modifying this code as it can
			// lead to security vulnerabilities.
			if _, err := setPermissionsForUser(ctx, userPerms); err != nil {
				// ignore NotImplementedError as the UserPermissionsStore is not implemented
				if _, ok := err.(*sourcegraph.NotImplementedError); !ok {
					return nil, err
				}
			}
		}
	}
	return userPerms, nil
}

// linkLDAPUserAccount links the local LDAP account with an account on the root
// server, such that the UID will be shared on both accounts.
// If an account exists on the root that shares an email address with the LDAP
// user, that account will be linked. Otherwise, a new account will be generated
// on the root server. The new account will have a random password, which can
// be reset via the forgot password link on the root server.
func linkLDAPUserAccount(ctx context.Context, ldapuser *ldap.LDAPUser) (*sourcegraph.User, error) {
	if len(ldapuser.Emails) == 0 {
		return nil, grpc.Errorf(codes.FailedPrecondition, "LDAP accounts must have an associated email address to access Sourcegraph")
	}
	ctx2 := fed.Config.NewRemoteContext(ctx)
	cl2 := sourcegraph.NewClientFromContext(ctx2)

	var federatedUser *sourcegraph.User
	var commonEmail string
	var err error

	// First check if a federated user with common a email exists
	for _, e := range ldapuser.Emails {
		federatedUser, err = cl2.Users.GetWithEmail(ctx2, &sourcegraph.EmailAddr{Email: e})
		if err != nil && grpc.Code(err) == codes.NotFound {
			continue
		} else if err != nil {
			return nil, err
		}
		commonEmail = e
		// TODO: send email to `commonEmail` address saying that an LDAP
		// account was linked to their sourcegraph.com account.
		break
	}

	// Append a random suffix to avoid name collisions on root server
	federatedLogin := ldapuser.Username + "_" + randstring.NewLen(6)
	federatedPassword := randstring.NewLen(20)

	if federatedUser == nil {
		// Federated user does not exist, so create a new user on root
		// Use any of the LDAP user's emails as the primary email.
		commonEmail = ldapuser.Emails[0]
		federatedUserSpec, err := cl2.Accounts.Create(ctx, &sourcegraph.NewAccount{
			// Use a non-colliding username.
			Login: federatedLogin,
			// Use the common email address as the primary email.
			Email: commonEmail,
			// Set a random password. The user can request password reset.
			Password: federatedPassword,
		})
		if err != nil {
			return nil, err
		}
		// Save the UID for linking with local user account.
		federatedUser = &sourcegraph.User{UID: federatedUserSpec.UID}
		// TODO: send email to `commonEmail` address saying that a sourcegraph.com
		// account was created for them and linked with their LDAP account.
	}

	// Now link local LDAP account with existing federated user
	userSpec, err := svc.Accounts(ctx).Create(ctx, &sourcegraph.NewAccount{
		// Use the LDAP username.
		Login: ldapuser.Username,
		// Use the common email address as the primary email.
		Email: commonEmail,
		// Share the UID to link the accounts.
		UID: federatedUser.UID,
		// Password in local store is irrelevant since auth will be done via LDAP.
		Password: federatedPassword,
	})
	if userSpec.UID != federatedUser.UID {
		return nil, grpc.Errorf(codes.Internal, "linked account UIDs do not match")
	}
	return &sourcegraph.User{
		UID:    userSpec.UID,
		Login:  userSpec.Login,
		Domain: userSpec.Domain,
	}, err
}
