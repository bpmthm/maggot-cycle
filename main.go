package main

import (
	"context"
	"fmt"
	"log"
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

// --- CONFIG ---
const (
	MinioEndpoint = "localhost:9000"
	MinioUser     = "admin"
	MinioPass     = "password123"
	BucketName    = "waste-photos"
	JWTSecret     = "rahasia-negara-maggot" // Nanti pindahin ke .env biar aman
)

// --- DATABASE MODELS ---
type User struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Email     string    `gorm:"unique" json:"email"`
	Password  string    `json:"-"` // "-" artinya password ga bakal ditampilin di JSON response
	Role      string    `json:"role" gorm:"default:user"` // user, admin, petugas
	CreatedAt time.Time `json:"created_at"`
}

type Waste struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `json:"user_id"` // Konek ke User yang upload
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
	dsn := "host=localhost user=postgres password=rahasia dbname=waste_db port=5432 sslmode=disable TimeZone=Asia/Jakarta"
	var err error
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("❌ Database Error:", err)
	}
	// Migrate Tabel User & Waste
	DB.AutoMigrate(&User{}, &Waste{})

	// 2. KONEKSI MINIO
	MinioClient, err = minio.New(MinioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(MinioUser, MinioPass, ""),
		Secure: false,
	})
	if err != nil {
		log.Fatal("❌ MinIO Error:", err)
	}

	// 3. SETUP FIBER
	app := fiber.New(fiber.Config{
		BodyLimit: 50 * 1024 * 1024, // 50MB
	})
	app.Use(cors.New())

	api := app.Group("/api")

	// === ROUTE PUBLIC (Bisa diakses siapa aja) ===
	api.Post("/register", Register)
	api.Post("/login", Login)

	// === ROUTE PROTECTED (Harus Login / Punya Token) ===
	// Kita pasang satpam (Middleware) di sini
	wasteRoutes := api.Group("/waste", AuthMiddleware)

	wasteRoutes.Post("/", CreateWaste) // Jadi: POST /api/waste/
	wasteRoutes.Get("/", GetWastes)    // Jadi: GET /api/waste/

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

	// Hash Password biar ga kebaca manusia
	hashedPassword, _ := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)

	user := User{
		Email:    input.Email,
		Password: string(hashedPassword),
	}

	if result := DB.Create(&user); result.Error != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Gagal daftar (Email udah ada?)"})
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

	// 1. Cari User by Email
	var user User
	if err := DB.Where("email = ?", input.Email).First(&user).Error; err != nil {
		return c.Status(401).JSON(fiber.Map{"error": "Email atau Password salah"})
	}

	// 2. Cek Password (Hash vs Input)
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(input.Password)); err != nil {
		return c.Status(401).JSON(fiber.Map{"error": "Email atau Password salah"})
	}

	// 3. Bikin Token JWT
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": user.ID,
		"role":    user.Role,
		"exp":     time.Now().Add(time.Hour * 72).Unix(), // Token basi dalam 3 hari
	})

	t, err := token.SignedString([]byte(JWTSecret))
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Gagal bikin token"})
	}

	return c.JSON(fiber.Map{"token": t})
}

func CreateWaste(c *fiber.Ctx) error {
	// Ambil ID user dari Token JWT (disimpen Middleware)
	userID := c.Locals("user_id").(float64) // JWT nyimpen angka sbg float

	waste := new(Waste)
	if err := c.BodyParser(waste); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Data text ga valid"})
	}

	file, err := c.FormFile("foto")
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Foto wajib diupload!"})
	}

	filename := uuid.New().String() + filepath.Ext(file.Filename)
	fileBuffer, _ := file.Open()
	defer fileBuffer.Close()

	_, err = MinioClient.PutObject(context.Background(), BucketName, filename, fileBuffer, file.Size, minio.PutObjectOptions{
		ContentType: file.Header.Get("Content-Type"),
	})
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Gagal upload ke MinIO"})
	}

	waste.FotoURL = fmt.Sprintf("http://%s/%s/%s", MinioEndpoint, BucketName, filename)
	waste.UserID = uint(userID) // Set pemilik sampah

	DB.Create(&waste)

	return c.Status(201).JSON(fiber.Map{"message": "Laporan sukses!", "data": waste})
}

func GetWastes(c *fiber.Ctx) error {
	var wastes []Waste
	// Preload("User") itu kayak JOIN table, biar ketauan siapa yg upload
	DB.Preload("User").Order("created_at desc").Find(&wastes)
	return c.JSON(fiber.Map{"data": wastes})
}

// --- MIDDLEWARE (SATPAM) ---
func AuthMiddleware(c *fiber.Ctx) error {
	// Ambil header Authorization: Bearer <token>
	authHeader := c.Get("Authorization")
	if authHeader == "" {
		return c.Status(401).JSON(fiber.Map{"error": "Mana Tokennya? Login dulu bos!"})
	}

	tokenString := strings.Replace(authHeader, "Bearer ", "", 1)

	// Validasi Token
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return []byte(JWTSecret), nil
	})

	if err != nil || !token.Valid {
		return c.Status(401).JSON(fiber.Map{"error": "Token Gak Valid / Udah Basi"})
	}

	// Simpen data user ke context biar bisa dipake di controller
	claims := token.Claims.(jwt.MapClaims)
	c.Locals("user_id", claims["user_id"])
	c.Locals("role", claims["role"])

	return c.Next() // Lanjut ke Controller
}