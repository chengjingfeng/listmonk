package main

import (
	"bytes"
)

const (
	notifTplImport   = "import-status"
	notifTplCampaign = "campaign-status"
)

// sendNotification sends out an e-mail notification to admins.
func sendNotification(tpl, subject string, data map[string]interface{}, app *App) error {
	data["RootURL"] = app.Constants.RootURL

	var b bytes.Buffer
	err := app.NotifTpls.ExecuteTemplate(&b, tpl, data)
	if err != nil {
		return err
	}

	err = app.Messenger.Push(app.Constants.FromEmail,
		app.Constants.NotifyEmails,
		subject,
		b.Bytes())
	if err != nil {
		app.Logger.Printf("error sending admin notification (%s): %v", subject, err)
		return err
	}

	return nil
}
