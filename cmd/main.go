package main

import (
	"encoding/json"
	"fmt"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/nats-io/stan.go"
	"github.com/spf13/viper"
	"log"
	"net/http"
)

type Order struct {
	OrderUID          string   `db:"order_uid" json:"order_uid"`
	TrackNumber       string   `db:"track_number" json:"track_number"`
	Entry             string   `db:"entry" json:"entry"`
	Delivery          Delivery `json:"delivery"`
	Payment           Payment  `json:"payment"`
	Items             []Item   `json:"items"`
	Locale            string   `db:"locale" json:"locale"`
	InternalSignature string   `db:"internal_signature" json:"internal_signature"`
	CustomerID        string   `db:"customer_id" json:"customer_id"`
	DeliveryService   string   `db:"delivery_service" json:"delivery_service"`
	Shardkey          string   `db:"shardkey" json:"shardkey"`
	SmID              int      `db:"sm_id" json:"sm_id"`
	DateCreated       string   `db:"date_created" json:"date_created"`
	OofShard          string   `db:"oof_shard" json:"oof_shard"`
}

type Delivery struct {
	OrderUID string `db:"order_uid" json:"-"`
	Name     string `db:"name" json:"name"`
	Phone    string `db:"phone" json:"phone"`
	Zip      string `db:"zip" json:"zip"`
	City     string `db:"city" json:"city"`
	Address  string `db:"address" json:"address"`
	Region   string `db:"region" json:"region"`
	Email    string `db:"email" json:"email"`
}

type Payment struct {
	OrderUID     string `db:"order_uid" json:"-"`
	Transaction  string `db:"transaction" json:"transaction"`
	RequestID    string `db:"request_id" json:"request_id"`
	Currency     string `db:"currency" json:"currency"`
	Provider     string `db:"provider" json:"provider"`
	Amount       int    `db:"amount" json:"amount"`
	PaymentDT    int    `db:"payment_dt" json:"payment_dt"`
	Bank         string `db:"bank" json:"bank"`
	DeliveryCost int    `db:"delivery_cost" json:"delivery_cost"`
	GoodsTotal   int    `db:"goods_total" json:"goods_total"`
	CustomFee    int    `db:"custom_fee" json:"custom_fee"`
}

type Item struct {
	ChrtID      int    `db:"chrt_id" json:"chrt_id"`
	OrderUID    string `db:"order_uid" json:"-"`
	TrackNumber string `db:"track_number" json:"track_number"`
	Price       int    `db:"price" json:"price"`
	Rid         string `db:"rid" json:"rid"`
	Name        string `db:"name" json:"name"`
	Sale        int    `db:"sale" json:"sale"`
	Size        string `db:"size" json:"size"`
	TotalPrice  int    `db:"total_price" json:"total_price"`
	NmID        int    `db:"nm_id" json:"nm_id"`
	Brand       string `db:"brand" json:"brand"`
	Status      int    `db:"status" json:"status"`
}

var db *sqlx.DB
var cache = make(map[string]Order)

func main() {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")

	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	dbConfig := fmt.Sprintf("user=%s password=%s dbname=%s sslmode=%s",
		viper.GetString("database.user"),
		viper.GetString("database.password"),
		viper.GetString("database.dbname"),
		viper.GetString("database.sslmode"))

	var err error
	db, err = sqlx.Open("postgres", dbConfig)
	if err != nil {
		log.Fatalf("Error opening database: %v", err)
	}

	sc, err := stan.Connect(
		viper.GetString("nats.cluster_id"),
		viper.GetString("nats.client_id"),
		stan.NatsURL(viper.GetString("nats.url")),
	)
	if err != nil {
		log.Fatalf("Error connecting to NATS Streaming: %v", err)
	}
	defer sc.Close()

	_, err = sc.Subscribe("orders", func(m *stan.Msg) {
		var order Order
		if err := json.Unmarshal(m.Data, &order); err != nil {
			log.Printf("Error unmarshaling message: %v", err)
			return
		}
		if validateOrder(order) {
			cache[order.OrderUID] = order
			saveToDB(order)
		} else {
			log.Printf("Invalid order data: %v", order)
		}
	})
	if err != nil {
		log.Fatalf("Error subscribing to channel: %v", err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})
	http.HandleFunc("/order", getOrderHandler)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func validateOrder(order Order) bool {
	if order.OrderUID == "" || order.TrackNumber == "" || order.Delivery.Name == "" || order.Payment.Transaction == "" {
		return false
	}
	for _, item := range order.Items {
		if item.ChrtID == 0 || item.Price == 0 || item.Name == "" {
			return false
		}
	}
	return true
}

func saveToDB(order Order) {
	tx, err := db.Beginx()
	if err != nil {
		log.Printf("Error starting transaction: %v", err)
		return
	}

	_, err = tx.NamedExec(`INSERT INTO orders (order_uid, track_number, entry, locale, internal_signature, customer_id, delivery_service, shardkey, sm_id, date_created, oof_shard)
                           VALUES (:order_uid, :track_number, :entry, :locale, :internal_signature, :customer_id, :delivery_service, :shardkey, :sm_id, :date_created, :oof_shard)`,
		order)
	if err != nil {
		tx.Rollback()
		log.Printf("Error inserting order: %v", err)
		return
	}

	order.Delivery.OrderUID = order.OrderUID
	_, err = tx.NamedExec(`INSERT INTO delivery (order_uid, name, phone, zip, city, address, region, email)
                           VALUES (:order_uid, :name, :phone, :zip, :city, :address, :region, :email)`,
		order.Delivery)
	if err != nil {
		tx.Rollback()
		log.Printf("Error inserting delivery: %v", err)
		return
	}

	order.Payment.OrderUID = order.OrderUID
	_, err = tx.NamedExec(`INSERT INTO payment (order_uid, transaction, request_id, currency, provider, amount, payment_dt, bank, delivery_cost, goods_total, custom_fee)
                           VALUES (:order_uid, :transaction, :request_id, :currency, :provider, :amount, :payment_dt, :bank, :delivery_cost, :goods_total, :custom_fee)`,
		order.Payment)
	if err != nil {
		tx.Rollback()
		log.Printf("Error inserting payment: %v", err)
		return
	}

	for _, item := range order.Items {
		item.OrderUID = order.OrderUID
		_, err = tx.NamedExec(`INSERT INTO item (chrt_id, order_uid, track_number, price, rid, name, sale, size, total_price, nm_id, brand, status)
                               VALUES (:chrt_id, :order_uid, :track_number, :price, :rid, :name, :sale, :size, :total_price, :nm_id, :brand, :status)`,
			item)
		if err != nil {
			tx.Rollback()
			log.Printf("Error inserting item: %v", err)
			return
		}
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("Error committing transaction: %v", err)
	}
}

func getOrderHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if order, found := cache[id]; found {
		json.NewEncoder(w).Encode(order)
	} else {
		var order Order
		err := db.Get(&order, `SELECT order_uid, track_number, entry, locale, internal_signature, customer_id, delivery_service, shardkey, sm_id, date_created, oof_shard FROM orders WHERE order_uid = $1`, id)
		if err != nil {
			http.Error(w, "Order not found", http.StatusNotFound)
			return
		}

		err = db.Get(&order.Delivery, `SELECT order_uid, name, phone, zip, city, address, region, email FROM delivery WHERE order_uid = $1`, id)
		if err != nil {
			http.Error(w, "Order not found", http.StatusNotFound)
			return
		}

		err = db.Get(&order.Payment, `SELECT order_uid, transaction, request_id, currency, provider, amount, payment_dt, bank, delivery_cost, goods_total, custom_fee FROM payment WHERE order_uid = $1`, id)
		if err != nil {
			http.Error(w, "Order not found", http.StatusNotFound)
			return
		}

		err = db.Select(&order.Items, `SELECT chrt_id, order_uid, track_number, price, rid, name, sale, size, total_price, nm_id, brand, status FROM item WHERE order_uid = $1`, id)
		if err != nil {
			http.Error(w, "Order not found", http.StatusNotFound)
			return
		}

		cache[id] = order
		json.NewEncoder(w).Encode(order)
	}
}
