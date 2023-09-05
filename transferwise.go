package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/jordan-wright/email"
	"github.com/mitchellh/mapstructure"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// transfer-wise api paths
const (
	transfersAPIPath      = "v1/transfers"
	quotesAPIPath         = "v2/quotes"
	liveRateAPIPath       = "v1/rates"
	cancelTransferAPIPath = "v1/transfers/{transferId}/cancel"
)

// transfer-wise hosts
const (
	hostProduction = "api.transferwise.com"
	hostSandbox    = "api.sandbox.transferwise.tech"
)

// fallback values for optional env variables
const (
	fallbackInterval = "1"
	fallbackMargin   = "0"
)

// SMTP mail server
const (
	smtpHost = "smtp.gmail.com"
	smtpPort = "587"
)

// other mail related constants
const (
	reminderMailSubject = "Reminder: Your transfer is about to expire"
	reminderMailBody    = "<h4>&#128184; The following transfer is going to expire on <b>%v</b></h4>" +
		"<ul> <li>Transfer ID: %v </li> <li> {%v} --> {%v} </li> <li> Booked Rate: %v </li> <li> Amount: %v %v </li> </ul>"
	expiryPeriodInHours = 36
)

// other constants
const PRODUCTION = "production"
const SANDBOX = "sandbox"

// error messages
const ErrNoCurrentTransferFound = "error: no current transfer found, please create a transfer before proceeding"
const ErrEnvVarMissingOrInvalid = "error: make sure env variables ENV, API_TOKEN are both provided and are valid"

// env vars
var envVar = getEnv("ENV", "")
var hostVar = getHost(envVar)
var apiTokenVar = getEnv("API_TOKEN", "")
var marginVar = getEnv("MARGIN", fallbackMargin)
var intervalVar = getEnv("INTERVAL", fallbackInterval)
var toEmailVar = getEnv("TO_MAIL", "")
var fromEmailVar = getEnv("FROM_MAIL", "")
var mailPassVar = getEnv("MAIL_PASS", "")

// HTTPClient interface
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

var (
	Client HTTPClient
)

func init() {
	Client = &http.Client{Timeout: 10 * time.Second}
}

func checkAndProcess() {
	if hostVar == "" || apiTokenVar == "" {
		log.Println(ErrEnvVarMissingOrInvalid)
		return
	}

	result, transfer, liveRate, err := compareRates()
	if err != nil {
		log.Println(err)
		return
	}
	if !result {
		log.Printf("|| NO ACTION NEEDED, Live Rate: %v || Transfer ID: %v | {%v} --> {%v} | Booked Rate: %v | Amount: %v ||",
			liveRate, transfer.Id, transfer.SourceCurrency, transfer.TargetCurrency, transfer.Rate, transfer.SourceAmount)
		return
	}

	newTransfer, err := createTransfer(transfer)
	if err != nil || !result {
		log.Println(err)
		return
	}

	log.Printf("|| NEW TRANSFER BOOKED || Transfer ID: %v | {%v} --> {%v} | Rate: %v |  Amount: %v ||",
		newTransfer.Id, newTransfer.SourceCurrency, newTransfer.TargetCurrency, newTransfer.Rate, newTransfer.SourceAmount)
}

// Send reminder mail in case the best quote is about to expire
func sendExpiryReminderMail() {
	empty := Transfer{}
	bookedTransfer, err := getBookedTransfer()
	if err != nil || bookedTransfer == empty {
		log.Printf("sendExpiryMail: %v", err)
	}

	quoteDetail, err := getDetailByQuoteId(bookedTransfer.QuoteUuid)
	if err != nil {
		log.Printf("sendExpiryMail: %v", err)
	}

	expiryTime, err := time.Parse(time.RFC3339, quoteDetail.RateExpirationTime)
	if err != nil {
		log.Printf("sendExpiryMail: %v", err)
	}

	if expiryTime.Sub(time.Now().UTC()).Hours() < expiryPeriodInHours {
		body := fmt.Sprintf(
			reminderMailBody,
			expiryTime.Format("2006-01-02 15:04:05 UTC"),
			bookedTransfer.Id,
			bookedTransfer.SourceCurrency,
			bookedTransfer.TargetCurrency,
			bookedTransfer.Rate,
			bookedTransfer.SourceCurrency,
			bookedTransfer.SourceAmount,
		)
		err := sendMail(reminderMailSubject, []byte(body))
		if err != nil {
			log.Printf("sendExpiryMail: %v", err)
		}
	}
	return
}

func compareRates() (result bool, bookedTransfer Transfer, currentRate float64, err error) {
	empty := Transfer{}
	bookedTransfer, err = getBookedTransfer()
	if err != nil || bookedTransfer == empty {
		return false, empty, 0, fmt.Errorf("compareRates: %v", err)
	}

	liveRate, err := getLiveRate(bookedTransfer.SourceCurrency, bookedTransfer.TargetCurrency)
	if err != nil || liveRate == 0 {
		return false, empty, 0, fmt.Errorf("compareRates: %v", err)
	}

	marginRate, err := strconv.ParseFloat(marginVar, 64)
	if err != nil {
		return false, empty, 0, fmt.Errorf("compareRates: %v", err)
	}

	bookedRate := bookedTransfer.Rate
	if liveRate > bookedRate && (liveRate-bookedRate >= marginRate) {
		return true, bookedTransfer, 0, nil
	}

	return false, bookedTransfer, liveRate, nil
}

func getBookedTransfer() (Transfer, error) {
	params := url.Values{"limit": {"3"}, "offset": {"0"}, "status": {"incoming_payment_waiting"}}
	url := &url.URL{RawQuery: params.Encode(), Host: hostVar, Scheme: "https", Path: transfersAPIPath}

	response, code, err := callExternalAPI(http.MethodGet, url.String(), nil)
	if err != nil || code != http.StatusOK {
		return Transfer{}, fmt.Errorf("error GET transfer list API: %v : %v", code, err)
	}

	var transfersList []Transfer
	err = mapstructure.Decode(response, &transfersList)
	if err != nil {
		return Transfer{}, fmt.Errorf("error decoding response: %v", err)
	}

	if len(transfersList) == 0 {
		return Transfer{}, fmt.Errorf(ErrNoCurrentTransferFound)
	}

	bookedTransfer := findBestTransfer(transfersList)
	quoteDetail, err := getDetailByQuoteId(bookedTransfer.QuoteUuid)
	if err != nil {
		return Transfer{}, fmt.Errorf("getBookedTransfer: %v", err)
	}
	bookedTransfer.SourceAmount = quoteDetail.SourceAmount
	bookedTransfer.Profile = quoteDetail.Profile

	return bookedTransfer, nil
}

func getLiveRate(source string, target string) (float64, error) {
	params := url.Values{"source": {source}, "target": {target}}
	url := &url.URL{RawQuery: params.Encode(), Host: hostVar, Scheme: "https", Path: liveRateAPIPath}

	response, code, err := callExternalAPI(http.MethodGet, url.String(), nil)
	if err != nil || code != http.StatusOK {
		return 0, fmt.Errorf("error GET live rate API: %v : %v", code, err)
	}

	var liveRate []LiveRate
	err = mapstructure.Decode(response, &liveRate)
	if err != nil {
		return 0, fmt.Errorf("error decoding live rate response: %v", err)
	}

	return liveRate[0].Rate, nil
}

func createTransfer(oldTransfer Transfer) (Transfer, error) {
	quoteId, err := generateQuote(oldTransfer.SourceCurrency, oldTransfer.TargetCurrency, oldTransfer.SourceAmount, oldTransfer.Profile)
	if err != nil {
		return Transfer{}, fmt.Errorf("createTransfer: %v", err)
	}

	createRequest := CreateTransferRequest{
		TargetAccount:         oldTransfer.TargetAccount,
		QuoteUuid:             quoteId,
		CustomerTransactionId: uuid.New().String(),
		Details:               oldTransfer.Details,
	}
	request, _ := json.Marshal(createRequest)

	url := &url.URL{Host: hostVar, Scheme: "https", Path: transfersAPIPath}
	response, code, err := callExternalAPI(http.MethodPost, url.String(), request)
	if err != nil || code != http.StatusOK {
		return Transfer{}, fmt.Errorf("error POST create transfer API: %v : %v", code, err)
	}

	var newTransfer Transfer
	err = mapstructure.Decode(response, &newTransfer)
	if err != nil {
		return Transfer{}, fmt.Errorf("error decoding response: %v", err)
	}
	newTransfer.SourceAmount = oldTransfer.SourceAmount

	cancelResult, err := cancelTransfer(oldTransfer.Id)
	if !cancelResult || err != nil {
		log.Println("Error deleting old transfer")
	}

	return newTransfer, nil
}

func cancelTransfer(transferId uint64) (bool, error) {
	path := strings.Replace(cancelTransferAPIPath, "{transferId}", strconv.FormatUint(transferId, 10), 1)

	url := &url.URL{Host: hostVar, Scheme: "https", Path: path}
	_, code, err := callExternalAPI(http.MethodPut, url.String(), nil)
	if err != nil || code != http.StatusOK {
		return false, fmt.Errorf("error PUT cancel transfer API: %v : %v", code, err)
	}

	return true, nil
}

func generateQuote(source string, target string, sourceAmount float64, profile uint64) (string, error) {
	quoteRequest := CreateQuoteRequest{
		SourceCurrency: source,
		TargetCurrency: target,
		SourceAmount:   sourceAmount,
		Profile:        profile,
	}

	request, _ := json.Marshal(quoteRequest)

	url := &url.URL{Host: hostVar, Scheme: "https", Path: quotesAPIPath}
	response, code, err := callExternalAPI(http.MethodPost, url.String(), request)
	if err != nil || code != http.StatusOK {
		return "", fmt.Errorf("error POST quote API: %v : %v", code, err)
	}

	var quote QuoteDetail
	err = mapstructure.Decode(response, &quote)
	if err != nil {
		return "", fmt.Errorf("error decoding quote response: %v", err)
	}

	return quote.Id, nil
}

func getDetailByQuoteId(quoteUuid string) (QuoteDetail, error) {
	path := quotesAPIPath + "/" + quoteUuid
	url := &url.URL{Host: hostVar, Scheme: "https", Path: path}

	response, code, err := callExternalAPI(http.MethodGet, url.String(), nil)
	if err != nil || code != http.StatusOK {
		return QuoteDetail{}, fmt.Errorf("error GET quote detail API: %v : %v", code, err)
	}

	var quoteDetail QuoteDetail
	err = mapstructure.Decode(response, &quoteDetail)
	if err != nil {
		return QuoteDetail{}, fmt.Errorf("error decoding to quote detail: %v : %v", code, err)
	}

	for _, paymentOption := range quoteDetail.PaymentOptions {
		if !paymentOption.Disabled && paymentOption.PayOut == "BANK_TRANSFER" {
			quoteDetail.SourceAmount = paymentOption.SourceAmount
			break
		}
	}

	return quoteDetail, nil
}

func callExternalAPI(method string, url string, reqBody []byte) (response interface{}, code int, err error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("error creating external api request: %v", err)
	}
	req.Header.Add("Authorization", "Bearer "+apiTokenVar)
	req.Header.Add("Content-Type", "application/json")

	res, err := Client.Do(req)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("error calling external api: %v", err)
	}
	err = json.NewDecoder(res.Body).Decode(&response)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("error decoding json response: %v", err)
	}
	code = res.StatusCode
	_ = res.Body.Close()

	return
}

func findBestTransfer(transferList []Transfer) (bestTransfer Transfer) {
	for i := range transferList {
		if i == 0 || bestTransfer.Rate < transferList[i].Rate {
			bestTransfer = transferList[i]
		}
	}
	return
}

func sendMail(subject string, body []byte) (err error) {
	if toEmailVar == "" || fromEmailVar == "" || mailPassVar == "" {
		return fmt.Errorf("error: env vars TO_MAIL, FROM_MAIL, MAIL_PASS not found")
	}
	e := email.NewEmail()
	e.From = fmt.Sprintf(" Transferwisely <%s>", fromEmailVar)
	e.To = []string{toEmailVar}
	e.Subject = subject
	e.HTML = body
	err = e.Send(smtpHost+":"+smtpPort, smtp.PlainAuth("", fromEmailVar, mailPassVar, smtpHost))
	return
}

func getHost(envVar string) string {
	switch strings.ToLower(envVar) {
	case SANDBOX:
		return hostSandbox
	case PRODUCTION:
		return hostProduction
	default:
		return ""
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}

	return fallback
}

type Transfer struct {
	Id             uint64          `json:"id"`
	Profile        uint64          `json:"profile"`
	TargetAccount  uint64          `json:"targetAccount"`
	SourceAmount   float64         `json:"sourceAmount"`
	Rate           float64         `json:"rate"`
	QuoteUuid      string          `json:"quote"`
	SourceCurrency string          `json:"sourceCurrency"`
	TargetCurrency string          `json:"targetCurrency"`
	Details        TransferDetails `json:"details"`
}

type TransferDetails struct {
	Reference       string `json:"reference"`
	TransferPurpose string `json:"transferPurpose"`
	SourceOfFunds   string `json:"sourceOfFunds"`
}

type QuoteDetail struct {
	Id                 string           `json:"id"`
	Rate               float64          `json:"rate"`
	SourceAmount       float64          `json:"sourceAmount"`
	SourceCurrency     string           `json:"sourceCurrency"`
	TargetCurrency     string           `json:"targetCurrency"`
	Profile            uint64           `json:"profile"`
	RateExpirationTime string           `json:"rateExpirationTime"`
	PaymentOptions     []PaymentOptions `json:"paymentOptions"`
}

type PaymentOptions struct {
	Disabled     bool    `json:"disabled"`
	PayOut       string  `json:"payOut"`
	SourceAmount float64 `json:"sourceAmount"`
}

type LiveRate struct {
	Rate float64 `json:"rate"`
}

type CreateTransferRequest struct {
	TargetAccount         uint64          `json:"targetAccount"`
	QuoteUuid             string          `json:"quoteUuid"`
	CustomerTransactionId string          `json:"customerTransactionId"`
	Details               TransferDetails `json:"details"`
}

type CreateQuoteRequest struct {
	SourceCurrency string  `json:"sourceCurrency"`
	TargetCurrency string  `json:"targetCurrency"`
	SourceAmount   float64 `json:"sourceAmount"`
	Profile        uint64  `json:"profile"`
}
