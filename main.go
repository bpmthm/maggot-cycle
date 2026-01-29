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
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// --- CONSTANTS ---
var (
	DbHost        = getEnv("DB_HOST", "localhost")
	MinioEndpoint = getEnv("MINIO_ENDPOINT", "localhost:9000")
	WaGatewayURL  = "http://wa-gateway:3000/send/message"
	
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

var DB *gorm.DB
var MinioClient *minio.Client

func main() {
	// 1. KONEKSI DB
	dsn := fmt.Sprintf("host=%s user=postgres password=rahasia dbname=waste_db port=5432 sslmode=disable TimeZone=Asia/Jakarta", DbHost)
	var err error
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("❌ Database Error:", err)
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

	// Routes Auth
	api.Post("/register", Register)
	api.Post("/login", Login)
	
	// Routes Sampah (Butuh Login)
	wasteRoutes := api.Group("/waste", AuthMiddleware)
	wasteRoutes.Post("/", CreateWaste)
	wasteRoutes.Get("/", GetWastes)
	
	// 👇 INI DIA YANG KEMAREN ILANG! 👇
	wasteRoutes.Put("/:id/status", UpdateWasteStatus)

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
	userID := c.Locals("user_id").(float64) // JWT v5 default claim number is float64

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

	// Background WA
	go func() {
		pesan := fmt.Sprintf("📢 *Laporan Baru Masuk!*\n\nJenis: %s\nBerat: %.2f Kg\nOleh User ID: %d", waste.Jenis, waste.Berat, waste.UserID)
		SendWhatsApp("6289648186679", pesan, waste.FotoURL) 
	}()

	return c.Status(201).JSON(fiber.Map{"message": "Laporan sukses & WA terkirim!", "data": waste})
}

func GetWastes(c *fiber.Ctx) error {
	var wastes []Waste
	DB.Preload("User").Order("created_at desc").Find(&wastes)
	return c.JSON(fiber.Map{"data": wastes})
}

// 👇 INI DIA FUNGSI UPDATE STATUS YANG KEMAREN ILANG 👇
func UpdateWasteStatus(c *fiber.Ctx) error {
	id := c.Params("id")
	
	var waste Waste
	// Cari data berdasarkan ID
	if err := DB.First(&waste, id).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "Data sampah tidak ditemukan"})
	}

	// Update status jadi 'selesai'
	waste.Status = "selesai"
	DB.Save(&waste)

	return c.JSON(fiber.Map{
		"message": "Status berhasil diupdate jadi Selesai!",
		"data":    waste,
	})
}

// --- FUNGSI KIRIM WA ---
func SendWhatsApp(nomorTujuan string, pesan string, fotoURL string) {
	if !strings.HasSuffix(nomorTujuan, "@s.whatsapp.net") {
		nomorTujuan = nomorTujuan + "@s.whatsapp.net"
	}

	if fotoURL != "" {
		fmt.Println("📸 Mendownload gambar dari:", fotoURL)
		
		respImg, err := http.Get(fotoURL)
		if err != nil {
			fmt.Println("❌ Gagal download gambar:", err)
			return
		}
		defer respImg.Body.Close()

		body := &bytes.Buffer{}
		writer := multipart.NewWriter(body)

		_ = writer.WriteField("phone", nomorTujuan)
		_ = writer.WriteField("caption", pesan)
		_ = writer.WriteField("type", "image")

		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, "image", "bukti.jpg"))
		h.Set("Content-Type", "image/jpeg") 

		part, err := writer.CreatePart(h)
		if err != nil {
			return
		}
		io.Copy(part, respImg.Body)
		writer.Close()

		http.Post("http://wa-gateway:3000/send/image", writer.FormDataContentType(), body)
		fmt.Println("✅ WA Gambar Terkirim!")

	} else {
		// Mode Teks
		payload := map[string]interface{}{
			"phone":   nomorTujuan,
			"message": pesan,
			"type":    "text",
		}
		jsonPayload, _ := json.Marshal(payload)
		http.Post("http://wa-gateway:3000/send/message", "application/json", bytes.NewBuffer(jsonPayload))
		fmt.Println("✅ WA Teks Terkirim!")
	}
}

// --- MIDDLEWARE ---
func AuthMiddleware(c *fiber.Ctx) error {
	authHeader := c.Get("Authorization")
	if authHeader == "" {
		return c.Status(401).JSON(fiber.Map{"error": "Unauthorized"})
	}
	tokenString := strings.Replace(authHeader, "Bearer ", "", 1)
	
	// Parse Token
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