package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

const (
	storeID      = "62fa94df8c13af2e242eba16"
	storeBaseURL = "https://shop.amul.com"
	infoJSURL    = storeBaseURL + "/user/info.js?_v=%d"
	pincodeURL   = storeBaseURL + "/entity/pincode?limit=50&filters[0][field]=pincode&filters[0][value]=%s&filters[0][operator]=regex&cf_cache=1h"
	setPrefURL   = storeBaseURL + "/entity/ms.settings/_/setPreferences"
)

var substoreIDs map[string]string

func init() {
	data, err := os.ReadFile("substores.json")
	if err != nil {
		log.Fatal("Failed to read substores.json:", err)
	}
	var entries []struct {
		ID    string `json:"_id"`
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Fatal("Failed to parse substores.json:", err)
	}
	substoreIDs = make(map[string]string, len(entries))
	for _, e := range entries {
		substoreIDs[e.Alias] = e.ID
	}
}

var defaultHeaders = map[string]string{
	"accept":             "application/json, text/plain, */*",
	"accept-language":    "en-US,en;q=0.9",
	"cache-control":      "no-cache",
	"frontend":           "1",
	"pragma":             "no-cache",
	"priority":           "u=1, i",
	"referer":            storeBaseURL + "/",
	"sec-ch-ua":          `"Google Chrome";v="137", "Chromium";v="137", "Not/A)Brand";v="24"`,
	"sec-ch-ua-mobile":   "?0",
	"sec-ch-ua-platform": `"Linux"`,
	"sec-fetch-dest":     "empty",
	"sec-fetch-mode":     "cors",
	"sec-fetch-site":     "same-origin",
	"sec-gpc":            "1",
	"user-agent":         "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36",
}

type User struct {
	TelegramID      int64 `gorm:"primaryKey;autoIncrement:false"`
	Pincode         string
	Substore        string
	TrackingStyle   string `gorm:"default:notify"`
	DefaultMaxCount int    `gorm:"default:3"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (User) TableName() string { return "users" }

type TrackedProduct struct {
	ID             int64  `gorm:"primaryKey"`
	UserID         int64  `gorm:"uniqueIndex:idx_tracked_user_sku;not null"`
	SKU            string `gorm:"uniqueIndex:idx_tracked_user_sku"`
	Name           string
	RemainingCount int `gorm:"default:3"`
	MaxCount       int `gorm:"default:3"`
	CreatedAt      time.Time
}

func (TrackedProduct) TableName() string { return "tracked_products" }
func (TrackedProduct) BeforeCreate(tx *gorm.DB) error {
	tx.Statement.AddClause(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "sku"}},
		DoNothing: true,
	})
	return nil
}

type Favourite struct {
	ID        int64  `gorm:"primaryKey"`
	UserID    int64  `gorm:"uniqueIndex:idx_fav_user_sku;not null"`
	SKU       string `gorm:"uniqueIndex:idx_fav_user_sku"`
	Name      string
	CreatedAt time.Time
}

func (Favourite) TableName() string { return "favourites" }
func (Favourite) BeforeCreate(tx *gorm.DB) error {
	tx.Statement.AddClause(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "sku"}},
		DoNothing: true,
	})
	return nil
}

type ProductCache struct {
	Substore string `gorm:"primaryKey"`
	Data     string
	CachedAt time.Time
}

func (ProductCache) TableName() string { return "product_cache" }

type SessionCache struct {
	Substore   string `gorm:"primaryKey"`
	CookieData string
	Tid        string
	CreatedAt  time.Time
}

func (SessionCache) TableName() string { return "session_cache" }

type StockHistory struct {
	SKU               string `gorm:"primaryKey"`
	Substore          string `gorm:"primaryKey"`
	LastSeenInStockAt time.Time
}

func (StockHistory) TableName() string { return "stock_history" }

type cookieRecord struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure"`
	Expires  string `json:"expires"`
	HTTPOnly bool   `json:"http_only"`
}

type Product struct {
	SKU           string  `json:"sku"`
	Name          string  `json:"name"`
	Brand         string  `json:"brand"`
	Price         float64 `json:"price"`
	OriginalPrice float64 `json:"original_price"`
	ComparePrice  float64 `json:"compare_price"`
	Available     int     `json:"available"`
	InventoryQty  int     `json:"inventory_quantity"`
	Alias         string  `json:"alias"`
	CatalogOnly   bool    `json:"catalog_only"`
}

type ProductsResponse struct {
	Data []json.RawMessage `json:"data"`
}

type PincodeRecord struct {
	Substore string `json:"substore"`
}

type PincodeResponse struct {
	Records []PincodeRecord `json:"records"`
}

type AmulSession struct {
	jar      *cookiejar.Jar
	tid      string
	substore string
	mu       sync.Mutex
}

type App struct {
	db       *gorm.DB
	b        *bot.Bot
	sessions map[string]*AmulSession
	sessMu   sync.RWMutex
	cache    map[string][]Product
	cacheMu  sync.RWMutex
	botUser  string

	awaitingInput   map[int64]string
	awaitingInputMu sync.Mutex
}

func loadEnvToken(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	data, err := os.ReadFile(".env")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == key {
			return strings.TrimSpace(parts[1])
		}
	}
	return ""
}

func main() {
	token := loadEnvToken("BOT_TOKEN")
	if token == "" {
		log.Fatal("BOT_TOKEN environment variable required")
	}

	db, err := gorm.Open(sqlite.Open("data/amul.db?_journal_mode=WAL&_busy_timeout=5000"), &gorm.Config{
		Logger: logger.New(
			log.New(os.Stdout, "\r\n", log.LstdFlags),
			logger.Config{
				SlowThreshold:             200 * time.Millisecond,
				LogLevel:                  logger.Warn,
				IgnoreRecordNotFoundError: true,
				Colorful:                  true,
			},
		),
	})
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}

	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	db.AutoMigrate(&User{}, &TrackedProduct{}, &Favourite{}, &ProductCache{}, &SessionCache{}, &StockHistory{})

	// Deduplicate before creating unique indexes
	db.Exec(`DELETE FROM tracked_products WHERE id NOT IN (SELECT MIN(id) FROM tracked_products GROUP BY user_id, sku)`)
	db.Exec(`DELETE FROM favourites WHERE id NOT IN (SELECT MIN(id) FROM favourites GROUP BY user_id, sku)`)
	db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_tracked_user_sku ON tracked_products(user_id, sku)")
	db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_fav_user_sku ON favourites(user_id, sku)")

	b, err := bot.New(token)
	if err != nil {
		log.Fatal("Failed to create bot:", err)
	}

	app := &App{
		db:            db,
		b:             b,
		sessions:      make(map[string]*AmulSession),
		cache:         make(map[string][]Product),
		awaitingInput: make(map[int64]string),
	}

	me, err := b.GetMe(context.Background())
	if err != nil {
		log.Fatal("Failed to get bot info:", err)
	}
	app.botUser = me.Username

	app.setCommands(context.Background())

	app.registerHandlers()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Printf("Bot %s started", app.botUser)

	go app.stockChecker(ctx)

	b.Start(ctx)
}

func (app *App) registerHandlers() {
	app.b.RegisterHandler(bot.HandlerTypeMessageText, "/start", bot.MatchTypePrefix, app.handleStart)
	app.b.RegisterHandler(bot.HandlerTypeMessageText, "setpincode", bot.MatchTypeCommand, app.handleSetPincode)
	app.b.RegisterHandler(bot.HandlerTypeMessageText, "/pincode", bot.MatchTypeExact, app.handlePincode)
	app.b.RegisterHandler(bot.HandlerTypeMessageText, "/products", bot.MatchTypePrefix, app.handleProducts)
	app.b.RegisterHandler(bot.HandlerTypeMessageText, "/tracked", bot.MatchTypeExact, app.handleTracked)
	app.b.RegisterHandler(bot.HandlerTypeMessageText, "/favourites", bot.MatchTypeExact, app.handleFavourites)
	app.b.RegisterHandler(bot.HandlerTypeMessageText, "/settings", bot.MatchTypeExact, app.handleSettings)

	app.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "fav:", bot.MatchTypePrefix, app.handleFavCallback)
	app.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "trk:", bot.MatchTypePrefix, app.handleTrkCallback)
	app.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "view_tracked", bot.MatchTypeExact, app.handleViewTracked)
	app.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "view_favs", bot.MatchTypeExact, app.handleViewFavs)
	app.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "toggle_style", bot.MatchTypeExact, app.handleToggleStyle)
	app.b.RegisterHandler(bot.HandlerTypeCallbackQueryData, "set_maxcount", bot.MatchTypeExact, app.handleSetMaxCount)
	app.b.RegisterHandlerMatchFunc(func(update *models.Update) bool {
		if update.Message == nil || update.Message.Text == "" {
			return false
		}
		app.awaitingInputMu.Lock()
		_, ok := app.awaitingInput[update.Message.From.ID]
		app.awaitingInputMu.Unlock()
		return ok
	}, app.handleAwaitingInput)
}

func (app *App) setCommands(ctx context.Context) {
	commands := []models.BotCommand{
		{Command: "start", Description: "Start the bot"},
		{Command: "setpincode", Description: "Set your delivery pincode"},
		{Command: "pincode", Description: "View your current pincode"},
		{Command: "products", Description: "List available protein products"},
		{Command: "tracked", Description: "View your tracked products"},
		{Command: "favourites", Description: "View your favourite products"},
		{Command: "settings", Description: "Change your preferences"},
	}
	_, err := app.b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: commands})
	if err != nil {
		log.Printf("Failed to set bot commands: %v", err)
	}
}

func (app *App) send(ctx context.Context, chatID int64, text string, markup ...models.ReplyMarkup) {
	params := &bot.SendMessageParams{
		ChatID:    chatID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
	}
	if len(markup) > 0 && markup[0] != nil {
		params.ReplyMarkup = markup[0]
	}
	if _, err := app.b.SendMessage(ctx, params); err != nil {
		log.Printf("Error sending message to %d: %v", chatID, err)
	}
}

// ─── Database helpers ───

func (app *App) getUser(telegramID int64) (*User, error) {
	var u User
	err := app.db.First(&u, telegramID).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &u, err
}

func (app *App) ensureUser(telegramID int64) error {
	return app.db.FirstOrCreate(&User{TelegramID: telegramID}, &User{TelegramID: telegramID}).Error
}

func (app *App) updatePincode(telegramID int64, pincode, substore string) error {
	return app.db.Model(&User{}).Where("telegram_id = ?", telegramID).Updates(map[string]interface{}{
		"pincode":  pincode,
		"substore": substore,
	}).Error
}

func (app *App) getTracked(userID int64) ([]TrackedProduct, error) {
	var out []TrackedProduct
	err := app.db.Where("user_id = ?", userID).Order("created_at desc").Find(&out).Error
	return out, err
}

func (app *App) addTracked(userID int64, sku, name string) error {
	var u User
	if err := app.db.First(&u, userID).Error; err != nil {
		return app.db.Create(&TrackedProduct{UserID: userID, SKU: sku, Name: name, RemainingCount: 3, MaxCount: 3}).Error
	}
	return app.db.Create(&TrackedProduct{
		UserID:         userID,
		SKU:            sku,
		Name:           name,
		RemainingCount: u.DefaultMaxCount,
		MaxCount:       u.DefaultMaxCount,
	}).Error
}

func (app *App) removeTracked(userID int64, sku string) error {
	return app.db.Where("user_id = ? AND sku = ?", userID, sku).Delete(&TrackedProduct{}).Error
}

func (app *App) isTracked(userID int64, sku string) bool {
	var count int64
	app.db.Model(&TrackedProduct{}).Where("user_id = ? AND sku = ?", userID, sku).Count(&count)
	return count > 0
}

func (app *App) getFavs(userID int64) ([]Favourite, error) {
	var out []Favourite
	err := app.db.Where("user_id = ?", userID).Order("created_at desc").Find(&out).Error
	return out, err
}

func (app *App) addFav(userID int64, sku, name string) error {
	return app.db.Create(&Favourite{UserID: userID, SKU: sku, Name: name}).Error
}

func (app *App) removeFav(userID int64, sku string) error {
	return app.db.Where("user_id = ? AND sku = ?", userID, sku).Delete(&Favourite{}).Error
}

func (app *App) isFav(userID int64, sku string) bool {
	var count int64
	app.db.Model(&Favourite{}).Where("user_id = ? AND sku = ?", userID, sku).Count(&count)
	return count > 0
}

// ─── Handlers ───

func (app *App) handleStart(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := update.Message.From.ID
	text := strings.TrimSpace(update.Message.Text)

	// Deep link payload: /start fav_SKU or /start track_SKU
	if strings.Contains(text, "fav_") || strings.Contains(text, "track_") {
		parts := strings.Fields(text)
		if len(parts) == 2 {
			b.DeleteMessage(ctx, &bot.DeleteMessageParams{
				ChatID:    update.Message.Chat.ID,
				MessageID: update.Message.ID,
			})

			payload := parts[1]
			if strings.HasPrefix(payload, "fav_") {
				sku := strings.TrimPrefix(payload, "fav_")
				name := app.getProductName(userID, sku)
				if app.isFav(userID, sku) {
					app.removeFav(userID, sku)
					app.send(ctx, update.Message.Chat.ID, fmt.Sprintf(`Removed <b>%s</b> from favourites.`, html.EscapeString(name)))
				} else {
					app.addFav(userID, sku, name)
					app.send(ctx, update.Message.Chat.ID, fmt.Sprintf(`Added <b>%s</b> to favourites.`, html.EscapeString(name)))
				}
				return
			}
			if strings.HasPrefix(payload, "track_") {
				sku := strings.TrimPrefix(payload, "track_")
				name := app.getProductName(userID, sku)
				if app.isTracked(userID, sku) {
					app.removeTracked(userID, sku)
					app.send(ctx, update.Message.Chat.ID, fmt.Sprintf(`Stopped tracking <b>%s</b>.`, html.EscapeString(name)))
				} else {
					app.addTracked(userID, sku, name)
					app.send(ctx, update.Message.Chat.ID, fmt.Sprintf(`Now tracking <b>%s</b>!`, html.EscapeString(name)))
				}
				return
			}
		}
	}

	app.ensureUser(userID)
	app.send(ctx, update.Message.Chat.ID,
		`Welcome to Amul Notify Bot! 🐄

I can help you track protein product availability from Amul.

<b>Commands:</b>
/setpincode <code>&lt;pincode&gt;</code> - Set your delivery pincode
/pincode - View your current pincode
/products - List all protein products
          OR
/products <code>&lt;search&gt;</code> - Search for a specific product
/tracked - View your tracked products
/favourites - View your favourite products
/settings - Change your preferences

<i>Tip: Hold the command from the menu to instantly add the command.</i>

Get started by typing /products or simply explore available stock.`)
}

func (app *App) handleSetPincode(ctx context.Context, b *bot.Bot, update *models.Update) {
	payload := strings.TrimSpace(update.Message.Text)
	payload = strings.TrimPrefix(payload, "/setpincode")
	payload = strings.TrimSpace(payload)
	if payload == "" {
		app.send(ctx, update.Message.Chat.ID, "Usage: /setpincode <code>&lt;pincode&gt;</code>\n\nExample: /setpincode 110001")
		return
	}

	matched, _ := regexp.MatchString(`^\d{6}$`, payload)
	if !matched {
		app.send(ctx, update.Message.Chat.ID, "Invalid pincode. Please enter a 6-digit Indian pincode.")
		return
	}

	app.ensureUser(update.Message.From.ID)

	substore, err := searchPincode(payload)
	if err != nil {
		log.Printf("Pincode search error for %s: %v", payload, err)
		app.send(ctx, update.Message.Chat.ID, "Could not find this pincode. Please try a different one.")
		return
	}

	if err := app.updatePincode(update.Message.From.ID, payload, substore); err != nil {
		log.Printf("Error updating pincode: %v", err)
		app.send(ctx, update.Message.Chat.ID, "An error occurred. Please try again.")
		return
	}

	app.cacheMu.Lock()
	delete(app.cache, substore)
	app.cacheMu.Unlock()

	app.send(ctx, update.Message.Chat.ID,
		fmt.Sprintf("Pincode set to <code>%s</code>\nSubstore: <b>%s</b>\n\nUse /products to browse available products.", payload, substore))
}

func (app *App) handlePincode(ctx context.Context, b *bot.Bot, update *models.Update) {
	user, err := app.getUser(update.Message.From.ID)
	if err != nil || user == nil || user.Pincode == "" {
		app.send(ctx, update.Message.Chat.ID, `You haven't set a pincode yet. Use /setpincode <code>&lt;pincode&gt;</code>`)
		return
	}
	app.send(ctx, update.Message.Chat.ID,
		fmt.Sprintf("Your pincode: <code>%s</code>\nSubstore: <b>%s</b>", user.Pincode, user.Substore))
}

func newTrue() *bool {
	v := true
	return &v
}

func productLinks(botUser, sku string, isTracked, isFav bool) string {
	trackLabel := "[Track]"
	if isTracked {
		trackLabel = "[Untrack]"
	}
	favLabel := "[Favourite]"
	if isFav {
		favLabel = "[Unfavourite]"
	}
	return fmt.Sprintf(`<b><a href="https://t.me/%s?start=track_%s">%s</a></b> | <b><a href="https://t.me/%s?start=fav_%s">%s</a></b>`,
		botUser, sku, trackLabel, botUser, sku, favLabel)
}

func formatProductBlock(p Product, index int, lastSeen *time.Time, remaining ...int) string {
	url := fmt.Sprintf("https://shop.amul.com/en/product/%s", p.Alias)

	status := "No 🔴"
	if p.Available != 0 {
		status = "Yes 🟢"
	}

	qty := p.InventoryQty
	if qty < 0 {
		qty = 0
	}

	lines := []string{
		fmt.Sprintf("%d. <b><a href=\"%s\">%s</a></b>", index+1, html.EscapeString(url), html.EscapeString(p.Name)),
		`     Protein: <b>N/A</b>`,
		fmt.Sprintf("     Price: <b>%.0f</b>", p.Price),
		fmt.Sprintf("     In Stock: <b>%s</b>", status),
	}

	if lastSeen != nil {
		lines = append(lines, fmt.Sprintf("     Last InStock: <b>%s</b>", lastSeen.Format("02-01-2006, 03:04 PM")))
	}

	if len(remaining) > 0 {
		lines = append(lines, fmt.Sprintf("     Remaining Notifications: <b>%d</b>", remaining[0]))
	}

	lines = append(lines, fmt.Sprintf("     Available Quantity: <b>%d</b>", qty))

	return strings.Join(lines, "\n")
}

func (app *App) handleProducts(ctx context.Context, b *bot.Bot, update *models.Update) {
	user, err := app.getUser(update.Message.From.ID)
	if err != nil || user == nil || user.Pincode == "" {
		app.send(ctx, update.Message.Chat.ID, "Please set your pincode first using /setpincode <code>&lt;pincode&gt;</code>")
		return
	}

	query := strings.TrimSpace(strings.TrimPrefix(update.Message.Text, "/products"))

	products, err := app.fetchProducts(user.Substore)
	if err != nil {
		log.Printf("Error fetching products for %s: %v", user.Substore, err)
		app.send(ctx, update.Message.Chat.ID, "Failed to fetch products. Please try again later.")
		return
	}

	if len(products) == 0 {
		app.send(ctx, update.Message.Chat.ID, "No products found for your area.")
		return
	}

	if query != "" {
		re, err := regexp.Compile("(?i)" + strings.Join(strings.Split(regexp.QuoteMeta(query), ""), ".*?"))
		if err == nil {
			var filtered []Product
			for _, p := range products {
				if re.MatchString(p.Name) || re.MatchString(p.Alias) {
					filtered = append(filtered, p)
				}
			}
			products = filtered
			if len(products) == 0 {
				app.send(ctx, update.Message.Chat.ID, fmt.Sprintf("No products match <b>%s</b>.", html.EscapeString(query)))
				return
			}
		}
	}

	userID := update.Message.From.ID

	type block struct {
		text string
		sku  string
	}

	title := fmt.Sprintf("<b>Amul Protein Products</b> (%s - %s)", user.Pincode, html.EscapeString(user.Substore))

	var blocks []block
	for i, p := range products {
		text := formatProductBlock(p, i, app.getLastInStockAt(p.SKU, user.Substore)) + "\n" + productLinks(app.botUser, p.SKU, app.isTracked(userID, p.SKU), app.isFav(userID, p.SKU))
		blocks = append(blocks, block{text: text, sku: p.SKU})
	}

	const maxLen = 3800

	var textBuf strings.Builder
	msgCount := 0

	flush := func() {
		if textBuf.Len() == 0 {
			return
		}
		msg := textBuf.String()
		if msgCount == 0 {
			msg = title + "\n\n" + msg
		}
		params := &bot.SendMessageParams{
			ChatID:             update.Message.Chat.ID,
			Text:               msg,
			ParseMode:          models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: newTrue()},
		}
		if _, err := app.b.SendMessage(context.Background(), params); err != nil {
			log.Printf("SendMessage in products error: %v", err)
		}
		msgCount++
		textBuf.Reset()
	}

	for _, blk := range blocks {
		if textBuf.Len()+len(blk.text) > maxLen && textBuf.Len() > 0 {
			flush()
		}
		if textBuf.Len() > 0 {
			textBuf.WriteString("\n\n")
		}
		textBuf.WriteString(blk.text)
	}

	if textBuf.Len() > 0 {
		flush()
	}
}

func (app *App) handleTracked(ctx context.Context, b *bot.Bot, update *models.Update) {
	user, err := app.getUser(update.Message.From.ID)
	if err != nil || user == nil || user.Pincode == "" {
		app.send(ctx, update.Message.Chat.ID, "Please set your pincode first using /setpincode <code>&lt;pincode&gt;</code>")
		return
	}

	tracked, err := app.getTracked(user.TelegramID)
	if err != nil {
		app.send(ctx, update.Message.Chat.ID, "An error occurred.")
		return
	}
	if len(tracked) == 0 {
		app.send(ctx, update.Message.Chat.ID, "You are not tracking any products.")
		return
	}

	products, err := app.fetchProducts(user.Substore)
	if err != nil {
		app.send(ctx, update.Message.Chat.ID, "Failed to fetch products.")
		return
	}

	trackedMap := make(map[string]TrackedProduct)
	for _, t := range tracked {
		trackedMap[t.SKU] = t
	}

	userID := update.Message.From.ID

	type block struct {
		text string
		sku  string
	}

	var blocks []block
	for i, p := range products {
		if _, ok := trackedMap[p.SKU]; !ok {
			continue
		}
		tp := trackedMap[p.SKU]
		remaining := -1
		if user.TrackingStyle == "always" {
			remaining = tp.RemainingCount
		}
		isTrk := app.isTracked(userID, p.SKU)
		isF := app.isFav(userID, p.SKU)
		text := formatProductBlock(p, i, app.getLastInStockAt(p.SKU, user.Substore), remaining) + "\n" + productLinks(app.botUser, p.SKU, isTrk, isF)
		blocks = append(blocks, block{text: text, sku: p.SKU})
	}

	if len(blocks) == 0 {
		app.send(ctx, update.Message.Chat.ID, "Tracked products not found in current listings.")
		return
	}

	title := fmt.Sprintf("<b>Tracked Products</b> (%s - %s)", user.Pincode, html.EscapeString(user.Substore))

	const maxLen = 3800

	var textBuf strings.Builder
	msgCount := 0

	flush := func() {
		if textBuf.Len() == 0 {
			return
		}
		msg := textBuf.String()
		if msgCount == 0 {
			msg = title + "\n\n" + msg
		}
		params := &bot.SendMessageParams{
			ChatID:             update.Message.Chat.ID,
			Text:               msg,
			ParseMode:          models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: newTrue()},
		}
		if _, err := app.b.SendMessage(context.Background(), params); err != nil {
			log.Printf("SendMessage error: %v", err)
		}
		msgCount++
		textBuf.Reset()
	}

	for _, blk := range blocks {
		if textBuf.Len()+len(blk.text) > maxLen && textBuf.Len() > 0 {
			flush()
		}
		if textBuf.Len() > 0 {
			textBuf.WriteString("\n\n")
		}
		textBuf.WriteString(blk.text)
	}

	if textBuf.Len() > 0 {
		flush()
	}
}

func (app *App) handleFavourites(ctx context.Context, b *bot.Bot, update *models.Update) {
	user, err := app.getUser(update.Message.From.ID)
	if err != nil || user == nil || user.Pincode == "" {
		app.send(ctx, update.Message.Chat.ID, "Please set your pincode first using /setpincode <code>&lt;pincode&gt;</code>")
		return
	}

	favs, err := app.getFavs(user.TelegramID)
	if err != nil {
		app.send(ctx, update.Message.Chat.ID, "An error occurred.")
		return
	}
	if len(favs) == 0 {
		app.send(ctx, update.Message.Chat.ID, "You have no favourite products. Use /products to add some.")
		return
	}

	products, err := app.fetchProducts(user.Substore)
	if err != nil {
		app.send(ctx, update.Message.Chat.ID, "Failed to fetch products.")
		return
	}

	favMap := make(map[string]bool)
	for _, f := range favs {
		favMap[f.SKU] = true
	}

	userID := update.Message.From.ID

	type block struct {
		text string
		sku  string
	}

	var blocks []block
	for i, p := range products {
		if !favMap[p.SKU] {
			continue
		}
		isTrk := app.isTracked(userID, p.SKU)
		isF := app.isFav(userID, p.SKU)
		text := formatProductBlock(p, i, app.getLastInStockAt(p.SKU, user.Substore)) + "\n" + productLinks(app.botUser, p.SKU, isTrk, isF)
		blocks = append(blocks, block{text: text, sku: p.SKU})
	}

	if len(blocks) == 0 {
		app.send(ctx, update.Message.Chat.ID, "Favourite products not found in current listings.")
		return
	}

	title := fmt.Sprintf("<b>Tracked Products</b> (%s - %s)", user.Pincode, html.EscapeString(user.Substore))

	const maxLen = 3800

	var textBuf strings.Builder
	msgCount := 0

	flush := func() {
		if textBuf.Len() == 0 {
			return
		}
		msg := textBuf.String()
		if msgCount == 0 {
			msg = title + "\n\n" + msg
		}
		params := &bot.SendMessageParams{
			ChatID:             update.Message.Chat.ID,
			Text:               msg,
			ParseMode:          models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: newTrue()},
		}
		if _, err := app.b.SendMessage(context.Background(), params); err != nil {
			log.Printf("SendMessage error: %v", err)
		}
		msgCount++
		textBuf.Reset()
	}

	for _, blk := range blocks {
		if textBuf.Len()+len(blk.text) > maxLen && textBuf.Len() > 0 {
			flush()
		}
		if textBuf.Len() > 0 {
			textBuf.WriteString("\n\n")
		}
		textBuf.WriteString(blk.text)
	}

	if textBuf.Len() > 0 {
		flush()
	}
}

func (app *App) handleSettings(ctx context.Context, b *bot.Bot, update *models.Update) {
	user, err := app.getUser(update.Message.From.ID)
	if err != nil || user == nil {
		app.send(ctx, update.Message.Chat.ID, "Please use /start first.")
		return
	}

	pincodeDisplay := user.Pincode
	if pincodeDisplay == "" {
		pincodeDisplay = "Not set"
	}

	styleLabel := "🔔 Notify (limited)"
	if user.TrackingStyle == "always" {
		styleLabel = "♾️ Always notify"
	}

	text := fmt.Sprintf(`<b>Settings</b>

📍 Pincode: <code>%s</code>
🏪 Substore: <b>%s</b>
🔔 Mode: <b>%s</b>
🔢 Max notifications: <b>%d</b> per product

Use /setpincode to change your pincode.`, pincodeDisplay, user.Substore, styleLabel, user.DefaultMaxCount)

	params := &bot.SendMessageParams{
		ChatID:    update.Message.Chat.ID,
		Text:      text,
		ParseMode: models.ParseModeHTML,
		ReplyMarkup: models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{
				{
					{Text: "Toggle tracking mode", CallbackData: "toggle_style"},
					{Text: "Set max count", CallbackData: "set_maxcount"},
				},
				{
					{Text: "📋 Tracked Products", CallbackData: "view_tracked"},
					{Text: "❤️ Favourites", CallbackData: "view_favs"},
				},
			},
		},
	}
	app.b.SendMessage(ctx, params)
}

// ─── Callback handlers ───

func (app *App) getProductName(userID int64, sku string) string {
	user, err := app.getUser(userID)
	if err != nil || user == nil || user.Substore == "" {
		return "Unknown Product"
	}
	products, err := app.fetchProducts(user.Substore)
	if err != nil {
		return "Unknown Product"
	}
	for _, p := range products {
		if p.SKU == sku {
			return p.Name
		}
	}
	return "Unknown Product"
}

func (app *App) handleFavCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	cb := update.CallbackQuery
	data := cb.Data
	parts := strings.SplitN(data, ":", 2)
	if len(parts) < 2 {
		return
	}
	sku := parts[1]
	userID := cb.From.ID

	var answer string
	if app.isFav(userID, sku) {
		app.removeFav(userID, sku)
		answer = "Removed from favourites"
	} else {
		app.addFav(userID, sku, app.getProductName(userID, sku))
		answer = "Added to favourites"
	}
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
		Text:            answer,
		ShowAlert:       false,
	})
}

func (app *App) handleTrkCallback(ctx context.Context, b *bot.Bot, update *models.Update) {
	cb := update.CallbackQuery
	data := cb.Data
	parts := strings.SplitN(data, ":", 2)
	if len(parts) < 2 {
		return
	}
	sku := parts[1]
	userID := cb.From.ID

	var answer string
	if app.isTracked(userID, sku) {
		app.removeTracked(userID, sku)
		answer = "Tracking stopped"
	} else {
		app.addTracked(userID, sku, app.getProductName(userID, sku))
		answer = "Now tracking!"
	}
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
		Text:            answer,
		ShowAlert:       false,
	})
}

func (app *App) handleViewTracked(ctx context.Context, b *bot.Bot, update *models.Update) {
	cb := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
		Text:            "",
	})

	products, err := app.getTracked(cb.From.ID)
	if err != nil || len(products) == 0 {
		app.send(ctx, cb.Message.Message.Chat.ID, "No tracked products.")
		return
	}

	var sb strings.Builder
	sb.WriteString("<b>Tracked Products:</b>\n\n")
	for _, p := range products {
		sb.WriteString(fmt.Sprintf("🔔 <b>%s</b> - %d/%d\n", html.EscapeString(p.Name), p.RemainingCount, p.MaxCount))
	}
	app.send(ctx, cb.Message.Message.Chat.ID, sb.String())
}

func (app *App) handleViewFavs(ctx context.Context, b *bot.Bot, update *models.Update) {
	cb := update.CallbackQuery
	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
		Text:            "",
	})

	favs, err := app.getFavs(cb.From.ID)
	if err != nil || len(favs) == 0 {
		app.send(ctx, cb.Message.Message.Chat.ID, "No favourites.")
		return
	}

	var sb strings.Builder
	sb.WriteString("<b>Favourite Products:</b>\n\n")
	for _, f := range favs {
		sb.WriteString(fmt.Sprintf("❤️ <b>%s</b>\n", html.EscapeString(f.Name)))
	}
	app.send(ctx, cb.Message.Message.Chat.ID, sb.String())
}

func (app *App) handleToggleStyle(ctx context.Context, b *bot.Bot, update *models.Update) {
	cb := update.CallbackQuery
	userID := cb.From.ID

	var u User
	err := app.db.First(&u, userID).Error
	if err != nil {
		return
	}

	newStyle := "always"
	if u.TrackingStyle == "always" {
		newStyle = "notify"
	}

	app.db.Model(&User{}).Where("telegram_id = ?", userID).Update("tracking_style", newStyle)

	label := "Always notify"
	if newStyle == "notify" {
		label = "Notify (limited)"
	}

	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
		Text:            "Mode: " + label,
		ShowAlert:       false,
	})
}

func (app *App) handleSetMaxCount(ctx context.Context, b *bot.Bot, update *models.Update) {
	cb := update.CallbackQuery
	userID := cb.From.ID

	app.awaitingInputMu.Lock()
	app.awaitingInput[userID] = "maxcount"
	app.awaitingInputMu.Unlock()

	b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: cb.ID,
		Text:            "",
	})

	app.send(ctx, cb.Message.Message.Chat.ID, "Send me a number (1-50) for the max notification count per product.\n\nThis updates all your tracked products.")
}

func (app *App) handleAwaitingInput(ctx context.Context, b *bot.Bot, update *models.Update) {
	userID := update.Message.From.ID

	app.awaitingInputMu.Lock()
	action := app.awaitingInput[userID]
	delete(app.awaitingInput, userID)
	app.awaitingInputMu.Unlock()

	if action != "maxcount" {
		return
	}

	text := strings.TrimSpace(update.Message.Text)
	num, err := strconv.Atoi(text)
	if err != nil || num < 1 || num > 50 {
		app.send(ctx, update.Message.Chat.ID, "Invalid number. Please send a number between 1 and 50.\n\nUse /settings to try again.")
		return
	}

	app.db.Model(&User{}).Where("telegram_id = ?", userID).Update("default_max_count", num)
	app.db.Model(&TrackedProduct{}).Where("user_id = ?", userID).Updates(map[string]interface{}{
		"max_count":       num,
		"remaining_count": num,
	})

	app.send(ctx, update.Message.Chat.ID, fmt.Sprintf("Max notification count updated to <b>%d</b> for all tracked products.", num))
}

// ─── Amul API ───

func (app *App) getSession(substore string) *AmulSession {
	app.sessMu.RLock()
	s, ok := app.sessions[substore]
	app.sessMu.RUnlock()
	if ok {
		return s
	}

	app.sessMu.Lock()
	defer app.sessMu.Unlock()

	if s, ok := app.sessions[substore]; ok {
		return s
	}

	jar, _ := cookiejar.New(nil)
	s = &AmulSession{jar: jar, substore: substore, tid: ""}

	var sc SessionCache
	err := app.db.Where("substore = ?", substore).First(&sc).Error
	if err == nil && sc.Tid != "" {
		s.tid = sc.Tid
		var records []cookieRecord
		if json.Unmarshal([]byte(sc.CookieData), &records) == nil {
			var cookies []*http.Cookie
			for _, r := range records {
				expires, _ := time.Parse(time.RFC1123, r.Expires)
				cookies = append(cookies, &http.Cookie{
					Name:     r.Name,
					Value:    r.Value,
					Domain:   r.Domain,
					Path:     r.Path,
					Secure:   r.Secure,
					Expires:  expires,
					HttpOnly: r.HTTPOnly,
				})
			}
			u, _ := url.Parse(storeBaseURL)
			jar.SetCookies(u, cookies)
		}
		if s.tid != "" {
			log.Printf("Restored session from cache for %s", substore)
		}
	}

	if s.tid == "" {
		log.Printf("Initializing new session for %s", substore)
	}
	if err := s.ensureInitialized(); err != nil {
		log.Printf("Session init failed for %s: %v", substore, err)
	}

	app.sessions[substore] = s
	app.persistSession(substore, s)
	return s
}

func (app *App) persistSession(substore string, s *AmulSession) {
	s.mu.Lock()
	tid := s.tid
	cookies := s.jar.Cookies(&url.URL{Scheme: "https", Host: "shop.amul.com"})
	s.mu.Unlock()

	if tid == "" {
		return
	}

	var records []cookieRecord
	for _, c := range cookies {
		records = append(records, cookieRecord{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			Expires:  c.Expires.Format(time.RFC1123),
			HTTPOnly: c.HttpOnly,
		})
	}
	data, _ := json.Marshal(records)

	app.db.Where("substore = ?", substore).Delete(&SessionCache{})
	app.db.Create(&SessionCache{
		Substore:   substore,
		CookieData: string(data),
		Tid:        tid,
		CreatedAt:  time.Now(),
	})
}

func (s *AmulSession) ensureInitialized() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tid != "" {
		return nil
	}

	client := &http.Client{Jar: s.jar}

	req1, _ := http.NewRequest("GET", storeBaseURL+"/en/browse/protein", nil)
	for k, v := range defaultHeaders {
		req1.Header.Set(k, v)
	}
	resp1, err := client.Do(req1)
	if err != nil {
		return fmt.Errorf("browse request failed: %w", err)
	}
	resp1.Body.Close()

	infoURL := fmt.Sprintf(infoJSURL, time.Now().UnixMilli())
	req2, _ := http.NewRequest("GET", infoURL, nil)
	for k, v := range defaultHeaders {
		req2.Header.Set(k, v)
	}
	// First call uses "undefined" as sessionID (matches TypeScript behaviour)
	req2.Header.Set("tid", makeTID("undefined"))
	cookies := s.jar.Cookies(req2.URL)
	var cookieParts []string
	for _, c := range cookies {
		cookieParts = append(cookieParts, c.Name+"="+c.Value)
	}
	if len(cookieParts) > 0 {
		req2.Header.Set("cookie", strings.Join(cookieParts, "; "))
	}

	resp2, err := client.Do(req2)
	if err != nil {
		return fmt.Errorf("info request failed: %w", err)
	}
	defer resp2.Body.Close()

	var bodyBuf bytes.Buffer
	bodyBuf.ReadFrom(resp2.Body)
	body := bodyBuf.String()

	// Response format: session = { ...json... }
	body = strings.TrimPrefix(body, "session = ")
	var sessionInfo struct {
		Tid string `json:"tid"`
	}
	if err := json.Unmarshal([]byte(body), &sessionInfo); err != nil {
		return fmt.Errorf("could not parse info response: %w", err)
	}
	if sessionInfo.Tid == "" {
		return fmt.Errorf("tid not found in info response")
	}
	s.tid = sessionInfo.Tid

	if s.substore != "" {
		prefBody := fmt.Sprintf(`{"data":{"store":"%s"}}`, s.substore)
		req3, _ := http.NewRequest("PUT", setPrefURL, strings.NewReader(prefBody))
		for k, v := range defaultHeaders {
			req3.Header.Set(k, v)
		}
		req3.Header.Set("content-type", "application/json")
		req3.Header.Set("tid", makeTID(s.tid))
		cookies3 := s.jar.Cookies(req3.URL)
		var cp3 []string
		for _, c := range cookies3 {
			cp3 = append(cp3, c.Name+"="+c.Value)
		}
		if len(cp3) > 0 {
			req3.Header.Set("cookie", strings.Join(cp3, "; "))
		}
		resp3, err := http.DefaultClient.Do(req3)
		if err == nil {
			resp3.Body.Close()
		}
	}

	return nil
}

func searchPincode(pincode string) (string, error) {
	urlStr := fmt.Sprintf(pincodeURL, pincode)
	req, _ := http.NewRequest("GET", urlStr, nil)
	for k, v := range defaultHeaders {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("pincode search failed: %w", err)
	}
	defer resp.Body.Close()

	var result PincodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("pincode parse failed: %w", err)
	}
	if len(result.Records) == 0 {
		return "", fmt.Errorf("no records found for pincode %s", pincode)
	}
	return result.Records[0].Substore, nil
}

func buildProductsURL(substore string) string {
	id := substoreIDs[substore]
	u := storeBaseURL + "/api/1/entity/ms.products" +
		"?fields[name]=1" +
		"&fields[brand]=1" +
		"&fields[sku]=1" +
		"&fields[price]=1" +
		"&fields[original_price]=1" +
		"&fields[compare_price]=1" +
		"&fields[available]=1" +
		"&fields[inventory_quantity]=1" +
		"&fields[images]=1" +
		"&fields[alias]=1" +
		"&fields[catalog_only]=1" +
		"&fields[is_catalog]=1" +
		"&filters[0][field]=categories" +
		"&filters[0][value][0]=protein" +
		"&filters[0][operator]=in" +
		"&filters[0][original]=1" +
		"&facets=true" +
		"&facetgroup=default_category_facet" +
		"&limit=32" +
		"&total=1" +
		"&start=0" +
		"&v=5" +
		"&device_type=other"
	if id != "" {
		u += "&substore=" + id
	}
	return u
}

func (app *App) fetchProducts(substore string) ([]Product, error) {
	app.cacheMu.RLock()
	cached, ok := app.cache[substore]
	app.cacheMu.RUnlock()
	if ok && len(cached) > 0 {
		return cached, nil
	}

	var pc ProductCache
	err := app.db.Where("substore = ? AND cached_at > ?", substore, time.Now().Add(-5*time.Minute)).First(&pc).Error
	if err == nil {
		var products []Product
		if json.Unmarshal([]byte(pc.Data), &products) == nil && len(products) > 0 {
			app.cacheMu.Lock()
			app.cache[substore] = products
			app.cacheMu.Unlock()
			log.Printf("Loaded %d products from cache for %s", len(products), substore)
			return products, nil
		}
	}

	session := app.getSession(substore)
	if session.tid == "" {
		return nil, fmt.Errorf("session not initialized for %s", substore)
	}

	session.mu.Lock()
	tid := session.tid
	mCookies := session.jar.Cookies(&url.URL{Scheme: "https", Host: "shop.amul.com"})
	session.mu.Unlock()

	req, _ := http.NewRequest("GET", buildProductsURL(substore), nil)
	for k, v := range defaultHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("referer", storeBaseURL+"/en/browse/protein")
	req.Header.Set("tid", makeTID(tid))

	var cp []string
	for _, c := range mCookies {
		cp = append(cp, c.Name+"="+c.Value)
	}
	if len(cp) > 0 {
		req.Header.Set("cookie", strings.Join(cp, "; "))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("products request failed: %w", err)
	}
	defer resp.Body.Close()

	var rawResp ProductsResponse
	if err := json.NewDecoder(resp.Body).Decode(&rawResp); err != nil {
		return nil, fmt.Errorf("products parse failed: %w", err)
	}

	var products []Product
	for _, raw := range rawResp.Data {
		var p Product
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.SKU == "" || p.CatalogOnly {
			continue
		}
		p.Name = html.UnescapeString(p.Name)
		products = append(products, p)
	}

	if len(products) > 0 {
		app.cacheMu.Lock()
		app.cache[substore] = products
		app.cacheMu.Unlock()

		data, _ := json.Marshal(products)
		app.db.Where("substore = ?", substore).Delete(&ProductCache{})
		app.db.Create(&ProductCache{Substore: substore, Data: string(data), CachedAt: time.Now()})
	}

	log.Printf("Fetched %d products for substore %s", len(products), substore)
	return products, nil
}

func makeTID(sessionID string) string {
	ts := fmt.Sprintf("%d", time.Now().UnixMilli())
	r := fmt.Sprintf("%d", rand.Intn(1000))
	input := storeID + ":" + ts + ":" + r + ":" + sessionID
	hash := sha256.Sum256([]byte(input))
	return ts + ":" + r + ":" + hex.EncodeToString(hash[:])
}

// ─── Stock checker ───

func (app *App) getLastInStockAt(sku, substore string) *time.Time {
	var sh StockHistory
	err := app.db.Where("sku = ? AND substore = ?", sku, substore).First(&sh).Error
	if err != nil {
		return nil
	}
	return &sh.LastSeenInStockAt
}

func (app *App) stockChecker(ctx context.Context) {
	app.checkStock(ctx)
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			app.checkStock(ctx)
		}
	}
}

func (app *App) checkStock(ctx context.Context) {
	type entry struct {
		id            int64
		substore      string
		trackingStyle string
	}
	var users []entry

	rows, err := app.db.Raw("SELECT DISTINCT u.telegram_id, u.substore, u.tracking_style FROM users u INNER JOIN tracked_products t ON u.telegram_id = t.user_id WHERE u.substore != ''").Rows()
	if err != nil {
		log.Printf("Stock checker query error: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var e entry
		if rows.Scan(&e.id, &e.substore, &e.trackingStyle) == nil {
			users = append(users, e)
		}
	}
	if len(users) == 0 {
		return
	}

	substores := map[string]bool{}
	for _, u := range users {
		substores[u.substore] = true
	}

	for ss := range substores {
		products, err := app.fetchProducts(ss)
		if err != nil {
			log.Printf("Stock checker fetch error for %s: %v", ss, err)
			continue
		}

		available := 0
		for _, p := range products {
			if p.Available != 0 {
				available++
			}
		}
		log.Printf("[tick] %s: fetched %d products, %d available", ss, len(products), available)

		pmap := make(map[string]Product)
		for _, p := range products {
			pmap[p.SKU] = p
			if p.Available != 0 {
				app.db.Where("sku = ? AND substore = ?", p.SKU, ss).Delete(&StockHistory{})
				app.db.Create(&StockHistory{
					SKU:               p.SKU,
					Substore:          ss,
					LastSeenInStockAt: time.Now(),
				})
			}
		}

		for _, u := range users {
			if u.substore != ss {
				continue
			}

			tracked, err := app.getTracked(u.id)
			if err != nil {
				continue
			}

			for _, tp := range tracked {
				p, ok := pmap[tp.SKU]
				if !ok || p.Available == 0 {
					continue
				}

				if u.trackingStyle != "always" && tp.RemainingCount <= 0 {
					continue
				}

				if u.trackingStyle != "always" {
					newCount := tp.RemainingCount - 1
					app.db.Model(&TrackedProduct{}).Where("id = ?", tp.ID).Update("remaining_count", newCount)
					if newCount == 0 {
						app.db.Delete(&TrackedProduct{}, tp.ID)
					}
				}

				text := fmt.Sprintf("✅ <b>In Stock!</b>\n\n%s\n\nPrice: ₹%.0f\nQty: %d\n\n<a href='%s/en/product/%s'>View Product</a>",
					html.EscapeString(p.Name), p.Price, p.InventoryQty, storeBaseURL, p.Alias)
				app.b.SendMessage(ctx, &bot.SendMessageParams{
					ChatID:    u.id,
					Text:      text,
					ParseMode: models.ParseModeHTML,
				})
			}
		}
	}
}
