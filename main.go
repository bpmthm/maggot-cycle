package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"log"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// --- CONFIG HELPER ---
// Fungsi ini biar kodingan lo bisa jalan di Laptop (Localhost) DAN di Docker (Container)
// tanpa perlu ubah-ubah kodingan manual.
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// --- CONSTANTS ---
var (
	// Kalau jalan di Docker, dia bakal pake nama container. Kalau di laptop, pake localhost.
	DbHost       = getEnv("DB_HOST", "localhost")
	MinioEndpoint = getEnv("MINIO_ENDPOINT", "localhost:9000")
	WaGatewayURL  = "http://wa-gateway:3000/send/message" // URL internal container WA
	
	MinioUser     = "admin"
	MinioPass     = "password123"
	BucketName    = "waste-photos"
	JWTSecret     = "rahasia-negara-maggot" 
)

// --- DATABASE MODELS ---
type User struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Email     string    `gorm:"unique" json:"email"`
	Password  string    `json:"-"`
	Role      string    `json:"role" gorm:"default:user"`
	CreatedAt time.Time `json:"created_at"`
}

type Waste struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `json:"user_id"`
	Jenis     string    `json:"jenis" form:"jenis"`
	Berat     float64   `json:"berat" form:"berat"`
	FotoURL   string    `json:"foto_url"`
	Status    string    `json:"status" gorm:"default:pending"`
	CreatedAt time.Time `json:"created_at"`
}

// Struct khusus buat kirim ke API WhatsApp
type WaRequest struct {
	Phone   string `json:"phone"`
	Message string `json:"message"`
}

var DB *gorm.DB
var MinioClient *minio.Client

func main() {
	// 1. KONEKSI DB (Dinamic Host)
	dsn := fmt.Sprintf("host=%s user=postgres password=rahasia dbname=waste_db port=5432 sslmode=disable TimeZone=Asia/Jakarta", DbHost)
	var err error
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("❌ Database Error (Cek container db-master nyala gak?):", err)
	}
	DB.AutoMigrate(&User{}, &Waste{})
	fmt.Println("✅ Sukses konek ke Database:", DbHost)

	// 2. KONEKSI MINIO
	MinioClient, err = minio.New(MinioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(MinioUser, MinioPass, ""),
		Secure: false,
	})
	if err != nil {
		log.Fatal("❌ MinIO Error:", err)
	}
	fmt.Println("✅ Sukses konek ke MinIO:", MinioEndpoint)

	// 3. SETUP SERVER
	app := fiber.New(fiber.Config{
		BodyLimit: 50 * 1024 * 1024, // Limit Upload 50MB
	})
	app.Use(cors.New())

	api := app.Group("/api")

	// Routes
	api.Post("/register", Register)
	api.Post("/login", Login)
	
	wasteRoutes := api.Group("/waste", AuthMiddleware)
	wasteRoutes.Post("/", CreateWaste)
	wasteRoutes.Get("/", GetWastes)

	log.Fatal(app.Listen(":3000"))
}

// --- CONTROLLERS ---

func Register(c *fiber.Ctx) error {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&input); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Input ga valid"})
	}

	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	user := User{Email: input.Email, Password: string(hashedPassword)}

	if result := DB.Create(&user); result.Error != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Email sudah terdaftar"})
	}

	return c.JSON(fiber.Map{"message": "Register sukses!", "data": user})
}

func Login(c *fiber.Ctx) error {
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.BodyParser(&input); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Input ga valid"})
	}

	var user User
	if err := DB.Where("email = ?", input.Email).First(&user).Error; err != nil {
		return c.Status(401).JSON(fiber.Map{"error": "Email atau Password salah"})
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(input.Password)); err != nil {
		return c.Status(401).JSON(fiber.Map{"error": "Email atau Password salah"})
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": user.ID,
		"role":    user.Role,
		"exp":     time.Now().Add(time.Hour * 72).Unix(),
	})

	t, _ := token.SignedString([]byte(JWTSecret))
	return c.JSON(fiber.Map{"token": t})
}

func CreateWaste(c *fiber.Ctx) error {
	userID := c.Locals("user_id").(float64)

	waste := new(Waste)
	if err := c.BodyParser(waste); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Data ga valid"})
	}

	file, err := c.FormFile("foto")
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Foto wajib diupload!"})
	}

	// Upload ke MinIO
	filename := uuid.New().String() + filepath.Ext(file.Filename)
	fileBuffer, _ := file.Open()
	defer fileBuffer.Close()

	_, err = MinioClient.PutObject(context.Background(), BucketName, filename, fileBuffer, file.Size, minio.PutObjectOptions{
		ContentType: file.Header.Get("Content-Type"),
	})
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Gagal upload ke Storage"})
	}

	// Simpan ke DB
	waste.FotoURL = fmt.Sprintf("http://%s/%s/%s", MinioEndpoint, BucketName, filename)
	waste.UserID = uint(userID)
	DB.Create(&waste)

	// --- INTEGRASI WHATSAPP (New!) ---
	// Kita pake 'go func' (Goroutine) biar proses kirim WA jalan di background
	// Jadi user gak perlu nunggu loading WA terkirim.
	go func() {
        pesan := fmt.Sprintf("📢 *Laporan Baru Masuk!*\n\nJenis: %s\nBerat: %.2f Kg\nOleh User ID: %d", waste.Jenis, waste.Berat, waste.UserID)
        
        // PARAMETER KE-3: Masukin waste.FotoURL biar dikirim gambarnya!
        // Ganti nomornya ke nomor temen lo lagi
        SendWhatsApp("6289648186679", pesan, waste.FotoURL) 
    }()

    return c.Status(201).JSON(fiber.Map{"message": "Laporan sukses & WA terkirim!", "data": waste})
}

func GetWastes(c *fiber.Ctx) error {
	var wastes []Waste
	DB.Preload("User").Order("created_at desc").Find(&wastes)
	return c.JSON(fiber.Map{"data": wastes})
}

// --- FUNGSI KIRIM WA (ULTIMATE DEBUG VERSION) ---
func SendWhatsApp(nomorTujuan string, pesan string, fotoURL string) {
    if !strings.HasSuffix(nomorTujuan, "@s.whatsapp.net") {
        nomorTujuan = nomorTujuan + "@s.whatsapp.net"
    }

    if fotoURL != "" {
        fmt.Println("📸 Mendownload gambar dari:", fotoURL)
        
        respImg, err := http.Get(fotoURL)
        if err != nil {
            fmt.Println("❌ Gagal download gambar dari MinIO:", err)
            return
        }
        defer respImg.Body.Close()

        body := &bytes.Buffer{}
        writer := multipart.NewWriter(body)

        // 1. Data Biasa
        _ = writer.WriteField("phone", nomorTujuan)
        _ = writer.WriteField("caption", pesan)
        _ = writer.WriteField("type", "image")

        // 2. BAGIAN INI KITA OPREK MANUAL (Biar ada Content-Type)
        // Kita bikin header manual biar server percaya ini beneran gambar
        h := make(textproto.MIMEHeader)
        h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, "image", "bukti_sampah.jpg"))
        h.Set("Content-Type", "image/jpeg") // <--- INI "KTP" YANG DIA CARI!

        part, err := writer.CreatePart(h)
        if err != nil {
            fmt.Println("❌ Gagal bikin part file:", err)
            return
        }

        // Salin isi gambar
        _, err = io.Copy(part, respImg.Body)
        writer.Close()

        // 3. Kirim
        resp, err := http.Post("http://wa-gateway:3000/send/image", writer.FormDataContentType(), body)
        if err != nil {
            fmt.Println("❌ Gagal kirim ke WA Gateway:", err)
            return
        }
        defer resp.Body.Close()
        
        respBody, _ := io.ReadAll(resp.Body)
        if resp.StatusCode != 200 {
            fmt.Printf("⚠️ WA Nolak (Status %d). Alasannya: %s\n", resp.StatusCode, string(respBody))
        } else {
            fmt.Println("✅ WA Gambar Sukses Terkirim!")
        }

    } else {
        // Mode Teks Biasa
        payload := map[string]interface{}{
            "phone":   nomorTujuan,
            "message": pesan,
            "type":    "text",
        }
        jsonPayload, _ := json.Marshal(payload)
        http.Post("http://wa-gateway:3000/send/message", "application/json", bytes.NewBuffer(jsonPayload))
        fmt.Println("✅ WA Teks Sukses Terkirim!")
    }
}
// --- MIDDLEWARE ---
func AuthMiddleware(c *fiber.Ctx) error {
	authHeader := c.Get("Authorization")
	if authHeader == "" {
		return c.Status(401).JSON(fiber.Map{"error": "Unauthorized"})
	}
	tokenString := strings.Replace(authHeader, "Bearer ", "", 1)
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return []byte(JWTSecret), nil
	})
	if err != nil || !token.Valid {
		return c.Status(401).JSON(fiber.Map{"error": "Token Invalid"})
	}
	claims := token.Claims.(jwt.MapClaims)
	c.Locals("user_id", claims["user_id"])
	c.Locals("role", claims["role"])
	return c.Next()
}