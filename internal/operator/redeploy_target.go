package operator

import "errors"

// checkRedeployImages accepts exactly one digest-pinned replacement image. A
// redeploy replaces one service; it never combines an app and a Tor change.
func checkRedeployImages(appImage, adminTorImage string) error {
	if (appImage == "") == (adminTorImage == "") {
		return errors.New("redeploy requires exactly one of --app-image or --admin-tor-image with an explicit sha256 digest")
	}
	if !imagePattern.MatchString(appImage + adminTorImage) {
		return errors.New("redeploy requires an image with an explicit sha256 digest")
	}
	return nil
}

// redeployTarget records the one replaced image and names its only Compose
// service. The administrator Tor identity volume, client snapshot, socket and
// every key stay as they are; only the tor-admin container is recreated.
func redeployTarget(c *Config, appImage, adminTorImage string) (service, done string, err error) {
	if adminTorImage == "" {
		c.AppImage = appImage
		return "app", "application redeployed; worker services, endpoints, keys and volumes retained", nil
	}
	if c.AdminTorImage == "" {
		return "", "", errors.New("this installation has no private administrator Tor service to redeploy")
	}
	c.AdminTorImage = adminTorImage
	return "tor-admin", "administrator Tor redeployed; application, identity volume, client authorization, endpoints and keys retained", nil
}
