package proxy

import (
	"fmt"
	"kiro-go/config"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

const (
	kiroStreamingSDKVersion = "1.0.34"
	kiroRuntimeSDKVersion   = "1.0.0"
)

type kiroHeaderValues struct {
	UserAgent    string
	AmzUserAgent string
	Host         string
}

func buildStreamingHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererstreaming", kiroStreamingSDKVersion, "m/E")
}

func buildRuntimeHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererruntime", kiroRuntimeSDKVersion, "m/N,E")
}

func buildKiroHeaderValues(account *config.Account, host, apiName, sdkVersion, mode string) kiroHeaderValues {
	clientCfg := config.GetKiroClientConfig()
	machineID := ""
	if account != nil {
		machineID = account.MachineId
	}

	userAgent := fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/%s#%s %s KiroIDE-%s",
		sdkVersion,
		clientCfg.SystemVersion,
		clientCfg.NodeVersion,
		apiName,
		sdkVersion,
		mode,
		clientCfg.KiroVersion,
	)
	amzUserAgent := fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s", sdkVersion, clientCfg.KiroVersion)
	if machineID != "" {
		userAgent += "-" + machineID
		amzUserAgent += "-" + machineID
	}

	return kiroHeaderValues{
		UserAgent:    userAgent,
		AmzUserAgent: amzUserAgent,
		Host:         host,
	}
}

func applyKiroBaseHeaders(req *http.Request, account *config.Account, values kiroHeaderValues) {
	if account != nil && account.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	}
	if isKiroAPIKeyAccount(account) {
		req.Header.Set("tokentype", "API_KEY")
	} else if account != nil && strings.EqualFold(strings.TrimSpace(account.AuthMethod), "external_idp") {
		req.Header.Set("TokenType", "EXTERNAL_IDP")
	}
	req.Header.Set("User-Agent", values.UserAgent)
	req.Header.Set("x-amz-user-agent", values.AmzUserAgent)
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	if values.Host != "" {
		req.Host = values.Host
	}
}

func applyKiroControlPlaneHeaders(req *http.Request, account *config.Account) {
	clientCfg := config.GetKiroClientConfig()
	host := ""
	if req != nil && req.URL != nil {
		host = req.URL.Host
	}
	if account != nil && account.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	}
	req.Header.Set("User-Agent", fmt.Sprintf(
		"aws-sdk-js/1.0.0 ua/2.1 os/%s lang/js md/nodejs#%s api/kirocontrolplanebearer#1.0.0 m/N,E",
		clientCfg.SystemVersion,
		clientCfg.NodeVersion,
	))
	req.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.0")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Connection", "close")
	if host != "" {
		req.Host = host
	}
}
