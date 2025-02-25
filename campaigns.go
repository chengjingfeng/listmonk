package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/asaskevich/govalidator"
	"github.com/knadh/listmonk/models"
	"github.com/labstack/echo"
	"github.com/lib/pq"
	uuid "github.com/satori/go.uuid"
	null "gopkg.in/volatiletech/null.v6"
)

// campaignReq is a wrapper over the Campaign model.
type campaignReq struct {
	models.Campaign

	// This overrides Campaign.Lists to receive and
	// write a list of int IDs during creation and updation.
	// Campaign.Lists is JSONText for sending lists children
	// to the outside world.
	ListIDs pq.Int64Array `db:"-" json:"lists"`

	// This is only relevant to campaign test requests.
	SubscriberEmails pq.StringArray `json:"subscribers"`
}

type campaignStats struct {
	ID        int       `db:"id" json:"id"`
	Status    string    `db:"status" json:"status"`
	ToSend    int       `db:"to_send" json:"to_send"`
	Sent      int       `db:"sent" json:"sent"`
	Started   null.Time `db:"started_at" json:"started_at"`
	UpdatedAt null.Time `db:"updated_at" json:"updated_at"`
	Rate      float64   `json:"rate"`
}

type campsWrap struct {
	Results models.Campaigns `json:"results"`

	Query   string `json:"query"`
	Total   int    `json:"total"`
	PerPage int    `json:"per_page"`
	Page    int    `json:"page"`
}

var (
	regexFromAddress   = regexp.MustCompile(`(.+?)\s<(.+?)@(.+?)>`)
	regexFullTextQuery = regexp.MustCompile(`\s+`)
)

// handleGetCampaigns handles retrieval of campaigns.
func handleGetCampaigns(c echo.Context) error {
	var (
		app = c.Get("app").(*App)
		pg  = getPagination(c.QueryParams())
		out campsWrap

		id, _     = strconv.Atoi(c.Param("id"))
		status    = c.QueryParams()["status"]
		query     = strings.TrimSpace(c.FormValue("query"))
		noBody, _ = strconv.ParseBool(c.QueryParam("no_body"))
		single    = false
	)

	// Fetch one list.
	if id > 0 {
		single = true
	}
	if query != "" {
		query = string(regexFullTextQuery.ReplaceAll([]byte(query), []byte("&")))
	}

	err := app.Queries.QueryCampaigns.Select(&out.Results, id, pq.StringArray(status), query, pg.Offset, pg.Limit)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error fetching campaigns: %s", pqErrMsg(err)))
	} else if single && len(out.Results) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "Campaign not found.")
	} else if len(out.Results) == 0 {
		out.Results = make([]models.Campaign, 0)
		return c.JSON(http.StatusOK, out)
	}

	for i := 0; i < len(out.Results); i++ {
		// Replace null tags.
		if out.Results[i].Tags == nil {
			out.Results[i].Tags = make(pq.StringArray, 0)
		}

		if noBody {
			out.Results[i].Body = ""
		}
	}

	// Lazy load stats.
	if err := out.Results.LoadStats(app.Queries.GetCampaignStats); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error fetching campaign stats: %v", pqErrMsg(err)))
	}

	if single {
		return c.JSON(http.StatusOK, okResp{out.Results[0]})
	}

	// Meta.
	out.Total = out.Results[0].Total
	out.Page = pg.Page
	out.PerPage = pg.PerPage

	return c.JSON(http.StatusOK, okResp{out})
}

// handlePreviewTemplate renders the HTML preview of a campaign body.
func handlePreviewCampaign(c echo.Context) error {
	var (
		app   = c.Get("app").(*App)
		id, _ = strconv.Atoi(c.Param("id"))
		body  = c.FormValue("body")

		camp = &models.Campaign{}
	)

	if id < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID.")
	}

	err := app.Queries.GetCampaignForPreview.Get(camp, id)
	if err != nil {
		if err == sql.ErrNoRows {
			return echo.NewHTTPError(http.StatusBadRequest, "Campaign not found.")
		}

		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error fetching campaign: %s", pqErrMsg(err)))
	}

	var sub models.Subscriber
	// Get a random subscriber from the campaign.
	if err := app.Queries.GetOneCampaignSubscriber.Get(&sub, camp.ID); err != nil {
		if err == sql.ErrNoRows {
			// There's no subscriber. Mock one.
			sub = dummySubscriber
		} else {
			return echo.NewHTTPError(http.StatusInternalServerError,
				fmt.Sprintf("Error fetching subscriber: %s", pqErrMsg(err)))
		}
	}

	// Compile the template.
	if body != "" {
		camp.Body = body
	}

	if err := camp.CompileTemplate(app.Manager.TemplateFuncs(camp)); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("Error compiling template: %v", err))
	}

	// Render the message body.
	m := app.Manager.NewMessage(camp, &sub)
	if err := m.Render(); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("Error rendering message: %v", err))
	}

	return c.HTML(http.StatusOK, string(m.Body))
}

// handleCreateCampaign handles campaign creation.
// Newly created campaigns are always drafts.
func handleCreateCampaign(c echo.Context) error {
	var (
		app = c.Get("app").(*App)
		o   campaignReq
	)

	if err := c.Bind(&o); err != nil {
		return err
	}

	// Validate.
	if err := validateCampaignFields(o, app); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	if !app.Manager.HasMessenger(o.MessengerID) {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("Unknown messenger %s", o.MessengerID))
	}

	// Insert and read ID.
	var newID int
	if err := app.Queries.CreateCampaign.Get(&newID,
		uuid.NewV4(),
		o.Name,
		o.Subject,
		o.FromEmail,
		o.Body,
		o.ContentType,
		o.SendAt,
		pq.StringArray(normalizeTags(o.Tags)),
		"email",
		o.TemplateID,
		o.ListIDs,
	); err != nil {
		if err == sql.ErrNoRows {
			return echo.NewHTTPError(http.StatusBadRequest,
				"There aren't any subscribers in the target lists to create the campaign.")
		}

		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error creating campaign: %v", pqErrMsg(err)))
	}

	// Hand over to the GET handler to return the last insertion.
	c.SetParamNames("id")
	c.SetParamValues(fmt.Sprintf("%d", newID))

	return handleGetCampaigns(c)
}

// handleUpdateCampaign handles campaign modification.
// Campaigns that are done cannot be modified.
func handleUpdateCampaign(c echo.Context) error {
	var (
		app   = c.Get("app").(*App)
		id, _ = strconv.Atoi(c.Param("id"))
	)

	if id < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID.")
	}

	var cm models.Campaign
	if err := app.Queries.GetCampaign.Get(&cm, id); err != nil {
		if err == sql.ErrNoRows {
			return echo.NewHTTPError(http.StatusBadRequest, "Campaign not found.")
		}

		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error fetching campaign: %s", pqErrMsg(err)))
	}

	if isCampaignalMutable(cm.Status) {
		return echo.NewHTTPError(http.StatusBadRequest,
			"Cannot update a running or a finished campaign.")
	}

	// Incoming params.
	var o campaignReq
	if err := c.Bind(&o); err != nil {
		return err
	}

	if err := validateCampaignFields(o, app); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	res, err := app.Queries.UpdateCampaign.Exec(cm.ID,
		o.Name,
		o.Subject,
		o.FromEmail,
		o.Body,
		o.ContentType,
		o.SendAt,
		pq.StringArray(normalizeTags(o.Tags)),
		o.TemplateID,
		o.ListIDs)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error updating campaign: %s", pqErrMsg(err)))
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "Campaign not found.")
	}

	return handleGetCampaigns(c)
}

// handleUpdateCampaignStatus handles campaign status modification.
func handleUpdateCampaignStatus(c echo.Context) error {
	var (
		app   = c.Get("app").(*App)
		id, _ = strconv.Atoi(c.Param("id"))
	)

	if id < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID.")
	}

	var cm models.Campaign
	if err := app.Queries.GetCampaign.Get(&cm, id); err != nil {
		if err == sql.ErrNoRows {
			return echo.NewHTTPError(http.StatusBadRequest, "Campaign not found.")
		}

		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error fetching campaign: %s", pqErrMsg(err)))
	}

	// Incoming params.
	var o campaignReq
	if err := c.Bind(&o); err != nil {
		return err
	}

	errMsg := ""
	switch o.Status {
	case models.CampaignStatusDraft:
		if cm.Status != models.CampaignStatusScheduled {
			errMsg = "Only scheduled campaigns can be saved as drafts"
		}
	case models.CampaignStatusScheduled:
		if cm.Status != models.CampaignStatusDraft {
			errMsg = "Only draft campaigns can be scheduled"
		}
		if !cm.SendAt.Valid {
			errMsg = "Campaign needs a `send_at` date to be scheduled"
		}

	case models.CampaignStatusRunning:
		if cm.Status != models.CampaignStatusPaused && cm.Status != models.CampaignStatusDraft {
			errMsg = "Only paused campaigns and drafts can be started"
		}
	case models.CampaignStatusPaused:
		if cm.Status != models.CampaignStatusRunning {
			errMsg = "Only active campaigns can be paused"
		}
	case models.CampaignStatusCancelled:
		if cm.Status != models.CampaignStatusRunning && cm.Status != models.CampaignStatusPaused {
			errMsg = "Only active campaigns can be cancelled"
		}
	}

	if len(errMsg) > 0 {
		return echo.NewHTTPError(http.StatusBadRequest, errMsg)
	}

	res, err := app.Queries.UpdateCampaignStatus.Exec(cm.ID, o.Status)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error updating campaign: %s", pqErrMsg(err)))
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "Campaign not found.")
	}

	return handleGetCampaigns(c)
}

// handleDeleteCampaign handles campaign deletion.
// Only scheduled campaigns that have not started yet can be deleted.
func handleDeleteCampaign(c echo.Context) error {
	var (
		app   = c.Get("app").(*App)
		id, _ = strconv.Atoi(c.Param("id"))
	)

	if id < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid ID.")
	}

	var cm models.Campaign
	if err := app.Queries.GetCampaign.Get(&cm, id); err != nil {
		if err == sql.ErrNoRows {
			return echo.NewHTTPError(http.StatusBadRequest, "Campaign not found.")
		}

		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error fetching campaign: %s", pqErrMsg(err)))
	}

	// Only scheduled campaigns can be deleted.
	if cm.Status != models.CampaignStatusDraft &&
		cm.Status != models.CampaignStatusScheduled {
		return echo.NewHTTPError(http.StatusBadRequest,
			"Only campaigns that haven't been started can be deleted.")
	}

	if _, err := app.Queries.DeleteCampaign.Exec(cm.ID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error deleting campaign: %v", pqErrMsg(err)))
	}

	return c.JSON(http.StatusOK, okResp{true})
}

// handleGetRunningCampaignStats returns stats of a given set of campaign IDs.
func handleGetRunningCampaignStats(c echo.Context) error {
	var (
		app = c.Get("app").(*App)
		out []campaignStats
	)

	if err := app.Queries.GetCampaignStatus.Select(&out, models.CampaignStatusRunning); err != nil {
		if err == sql.ErrNoRows {
			return c.JSON(http.StatusOK, okResp{[]struct{}{}})
		}

		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error fetching campaign stats: %s", pqErrMsg(err)))
	} else if len(out) == 0 {
		return c.JSON(http.StatusOK, okResp{[]struct{}{}})
	}

	// Compute rate.
	for i, c := range out {
		if c.Started.Valid && c.UpdatedAt.Valid {
			diff := c.UpdatedAt.Time.Sub(c.Started.Time).Minutes()
			if diff > 0 {
				var (
					sent = float64(c.Sent)
					rate = sent / diff
				)
				if rate > sent || rate > float64(c.ToSend) {
					rate = sent
				}
				out[i].Rate = rate
			}
		}
	}

	return c.JSON(http.StatusOK, okResp{out})
}

// handleTestCampaign handles the sending of a campaign message to
// arbitrary subscribers for testing.
func handleTestCampaign(c echo.Context) error {
	var (
		app       = c.Get("app").(*App)
		campID, _ = strconv.Atoi(c.Param("id"))
		req       campaignReq
	)

	if campID < 1 {
		return echo.NewHTTPError(http.StatusBadRequest, "Invalid campaign ID.")
	}

	// Get and validate fields.
	if err := c.Bind(&req); err != nil {
		return err
	}
	// Validate.
	if err := validateCampaignFields(req, app); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if len(req.SubscriberEmails) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "No subscribers to target.")
	}

	// Get the subscribers.
	for i := 0; i < len(req.SubscriberEmails); i++ {
		req.SubscriberEmails[i] = strings.ToLower(strings.TrimSpace(req.SubscriberEmails[i]))
	}
	var subs models.Subscribers
	if err := app.Queries.GetSubscribersByEmails.Select(&subs, req.SubscriberEmails); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error fetching subscribers: %s", pqErrMsg(err)))
	} else if len(subs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "No known subscribers given.")
	}

	// The campaign.
	var camp models.Campaign
	if err := app.Queries.GetCampaignForPreview.Get(&camp, campID); err != nil {
		if err == sql.ErrNoRows {
			return echo.NewHTTPError(http.StatusBadRequest, "Campaign not found.")
		}
		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("Error fetching campaign: %s", pqErrMsg(err)))
	}

	// Override certain values in the DB with incoming values.
	camp.Name = req.Name
	camp.Subject = req.Subject
	camp.FromEmail = req.FromEmail
	camp.Body = req.Body

	// Send the test messages.
	for _, s := range subs {
		if err := sendTestMessage(&s, &camp, app); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("Error sending test: %v", err))
		}
	}

	return c.JSON(http.StatusOK, okResp{true})
}

// sendTestMessage takes a campaign and a subsriber and sends out a sample campain message.
func sendTestMessage(sub *models.Subscriber, camp *models.Campaign, app *App) error {
	if err := camp.CompileTemplate(app.Manager.TemplateFuncs(camp)); err != nil {
		return fmt.Errorf("Error compiling template: %v", err)
	}

	// Render the message body.
	m := app.Manager.NewMessage(camp, sub)
	if err := m.Render(); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("Error rendering message: %v", err))
	}

	if err := app.Messenger.Push(camp.FromEmail, []string{sub.Email}, camp.Subject, m.Body); err != nil {
		return err
	}

	return nil
}

// validateCampaignFields validates incoming campaign field values.
func validateCampaignFields(c campaignReq, app *App) error {
	if !regexFromAddress.Match([]byte(c.FromEmail)) {
		if !govalidator.IsEmail(c.FromEmail) {
			return errors.New("invalid `from_email`")
		}
	}

	if !govalidator.IsByteLength(c.Name, 1, stdInputMaxLen) {
		return errors.New("invalid length for `name`")
	}
	if !govalidator.IsByteLength(c.Subject, 1, stdInputMaxLen) {
		return errors.New("invalid length for `subject`")
	}

	// if !govalidator.IsByteLength(c.Body, 1, bodyMaxLen) {
	// 	return errors.New("invalid length for `body`")
	// }

	// If there's a "send_at" date, it should be in the future.
	if c.SendAt.Valid {
		if c.SendAt.Time.Before(time.Now()) {
			return errors.New("`send_at` date should be in the future")
		}
	}

	camp := models.Campaign{Body: c.Body, TemplateBody: tplTag}
	if err := c.CompileTemplate(app.Manager.TemplateFuncs(&camp)); err != nil {
		return fmt.Errorf("Error compiling campaign body: %v", err)
	}

	return nil
}

// isCampaignalMutable tells if a campaign's in a state where it's
// properties can be mutated.
func isCampaignalMutable(status string) bool {
	return status == models.CampaignStatusRunning ||
		status == models.CampaignStatusCancelled ||
		status == models.CampaignStatusFinished
}
