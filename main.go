package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
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
	// DbHost cuma dipake buat fallback kalau di localhost
	DbHost        = getEnv("DB_HOST", "localhost")
	MinioEndpoint = getEnv("MINIO_ENDPOINT", "localhost:9000")
	WaGatewayURL  = "http://wa-gateway:3000/send/message"

	MinioUser  = "admin"
	MinioPass  = "password123"
	BucketName = "waste-photos"
	JWTSecret  = "rahasia-negara-maggot"
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
	// 1. KONEKSI DB (LOGIC BARU: SMART DETECT)
	// Cek apakah ada environment variable DATABASE_URL (dari Neon/Render)
	databaseUrl := os.Getenv("DATABASE_URL")
	var dsn string

	if databaseUrl != "" {
		// Kalo ada (lagi di Cloud), pake connection string dari sana
		dsn = databaseUrl
		fmt.Println("☁️  Mendeteksi Environment Cloud. Menggunakan DATABASE_URL.")
	} else {
		// Kalo ga ada (lagi di Laptop), pake settingan manual localhost lo
		dsn = fmt.Sprintf("host=%s user=postgres password=rahasia dbname=waste_db port=5432 sslmode=disable TimeZone=Asia/Jakarta", DbHost)
		fmt.Println("🏠 Mendeteksi Environment Local. Menggunakan Localhost.")
	}

	var err error
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("❌ Database Error:", err)
	}
	
	// Auto Migrate (Bikin tabel otomatis kalo belum ada)
	err = DB.AutoMigrate(&User{}, &Waste{})
	if err != nil {
		log.Fatal("❌ Gagal Migrasi Tabel:", err)
	}
	fmt.Println("✅ Sukses konek ke Database!")

	// 2. KONEKSI MINIO
	// Note: Di Render gratisan, MinIO local ini mungkin gak jalan (perlu storage cloud kayak AWS S3/Supabase Storage).
	// Tapi biarin dulu kodingannya biar gak error compile. Nanti fitur upload mungkin mati sementara di cloud.
	MinioClient, err = minio.New(MinioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(MinioUser, MinioPass, ""),
		Secure: false,
	})
	if err != nil {
		log.Println("⚠️ MinIO Error (Fitur Upload mungkin terkendala):", err)
	} else {
		fmt.Println("✅ Sukses konek ke MinIO:", MinioEndpoint)
	}

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

	// Routes Update & Delete
	wasteRoutes.Put("/:id/status", UpdateWasteStatus)
	wasteRoutes.Delete("/:id", DeleteWaste)

	// Port dinamis (Render ngasih port acak di env PORT, kalo ga ada pake 3000)
	port := getEnv("PORT", "3000")
	fmt.Println("🚀 Server jalan di port:", port)
	log.Fatal(app.Listen(":" + port))
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
		// Log error tapi jangan crash, biar DB tetep kesimpen (opsional)
		fmt.Println("❌ Gagal upload MinIO:", err)
		return c.Status(500).JSON(fiber.Map{"error": "Gagal upload ke Storage"})
	}

	// Simpan ke DB
	// Note: Di local ini jalan, di Cloud mungkin perlu URL public bucket S3/MinIO
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

func UpdateWasteStatus(c *fiber.Ctx) error {
	id := c.Params("id")
	var waste Waste
	if err := DB.First(&waste, id).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "Data sampah tidak ditemukan"})
	}
	waste.Status = "selesai"
	DB.Save(&waste)
	return c.JSON(fiber.Map{"message": "Status berhasil diupdate jadi Selesai!", "data": waste})
}

func DeleteWaste(c *fiber.Ctx) error {
	id := c.Params("id")
	var waste Waste
	if err := DB.First(&waste, id).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "Data gak ketemu"})
	}
	if err := DB.Delete(&waste).Error; err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Gagal menghapus data"})
	}
	return c.JSON(fiber.Map{"message": "Data berhasil dihapus!"})
}

func SendWhatsApp(nomorTujuan string, pesan string, fotoURL string) {
	// Skip kirim WA kalau gatewaynya masih localhost dan kita lagi di cloud
	// (Kecuali lo punya WA gateway public)
	if strings.Contains(WaGatewayURL, "localhost") && os.Getenv("DATABASE_URL") != "" {
		fmt.Println("⚠️ Skip kirim WA karena di Cloud (WA Gateway Local)")
		return
	}

	if !strings.HasSuffix(nomorTujuan, "@s.whatsapp.net") {
		nomorTujuan = nomorTujuan + "@s.whatsapp.net"
	}

	if fotoURL != "" {
		respImg, err := http.Get(fotoURL)
		if err != nil {
			fmt.Println("❌ Gagal download gambar WA:", err)
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
		http.Post(WaGatewayURL, writer.FormDataContentType(), body)
	} else {
		payload := map[string]interface{}{"phone": nomorTujuan, "message": pesan, "type": "text"}
		jsonPayload, _ := json.Marshal(payload)
		http.Post(WaGatewayURL, "application/json", bytes.NewBuffer(jsonPayload))
	}
}

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