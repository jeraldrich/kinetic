package kinetic

import (
	"errors"

	gokinesis "github.com/rewardStyle/go-kinesis"
)

// ErrMetaAuthentication represents an error that occurred on authentication from meta
var ErrMetaAuthentication = errors.New("Authentication error: failed to auth from meta.  Your IAM roles are bad, or you need to specify an AccessKey and SecretKey")

func authenticate(accessKey, secretKey string) (auth *gokinesis.AuthCredentials, err error) {
	if accessKey == "" || secretKey == "" {
		if auth, err = gokinesis.NewAuthFromMetadata(); err != nil {
			return nil, ErrMetaAuthentication
		}
	} else {
		auth = gokinesis.NewAuth(accessKey, secretKey, "")
	}
	return
}
