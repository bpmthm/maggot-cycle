package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// --- CONFIG ---
const (
	MinioEndpoint = "localhost:9000" // Port API MinIO (Bukan Console)
	MinioUser     = "admin"
	MinioPass     = "password123"
	BucketName    = "waste-photos"
)

// --- MODEL DATABASE ---
type Waste struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Jenis     string    `json:"jenis" form:"jenis"` // Tag 'form' buat nerima dari Multipart
	Berat     float64   `json:"berat" form:"berat"`
	FotoURL   string    `json:"foto_url"`           // Kita simpen Link-nya aja
	Status    string    `json:"status" gorm:"default:pending"`
	CreatedAt time.Time `json:"created_at"`
}

var DB *gorm.DB
var MinioClient *minio.Client

func main() {
	// 1. KONEKSI DATABASE (Postgres)
	dsn := "host=localhost user=postgres password=rahasia dbname=waste_db port=5432 sslmode=disable TimeZone=Asia/Jakarta"
	var err error
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("❌ Database Error:", err)
	}
	DB.AutoMigrate(&Waste{})

	// 2. KONEKSI MINIO (Object Storage)
	MinioClient, err = minio.New(MinioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(MinioUser, MinioPass, ""),
		Secure: false, // Karena localhost gapake HTTPS
	})
	if err != nil {
		log.Fatal("❌ MinIO Error:", err)
	}
	fmt.Println("✅ Sukses konek ke Postgres & MinIO!")

	// 3. SETUP SERVER
	app := fiber.New(fiber.Config{
        BodyLimit: 50 * 1024 * 1024, 
	})
	app.Use(cors.New())
	api := app.Group("/api")

	// --- ENDPOINT UPLOAD SAMPAH + FOTO ---
	api.Post("/waste", func(c *fiber.Ctx) error {
		// A. Parse Data Text (Jenis & Berat)
		waste := new(Waste)
		if err := c.BodyParser(waste); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "Data text ga valid"})
		}

		// B. Tangkap File Foto dari Form
		file, err := c.FormFile("foto") // Nama field di HTML nanti harus name="foto"
		if err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "Foto wajib diupload!"})
		}

		// C. Upload ke MinIO
		// Bikin nama file unik pake UUID biar gak bentrok
		filename := uuid.New().String() + filepath.Ext(file.Filename)
		fileBuffer, _ := file.Open()
		defer fileBuffer.Close()

		_, err = MinioClient.PutObject(context.Background(), BucketName, filename, fileBuffer, file.Size, minio.PutObjectOptions{
			ContentType: file.Header.Get("Content-Type"),
		})
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "Gagal upload ke MinIO"})
		}

		// D. Simpan URL & Data ke Database
		// URL format: http://localhost:9000/nama-bucket/nama-file
		waste.FotoURL = fmt.Sprintf("http://%s/%s/%s", MinioEndpoint, BucketName, filename)
		
		DB.Create(&waste)

		return c.Status(201).JSON(fiber.Map{
			"message": "Laporan + Foto berhasil disimpan!",
			"data":    waste,
		})
	})

	// --- ENDPOINT LIHAT HISTORY ---
	api.Get("/waste", func(c *fiber.Ctx) error {
		var wastes []Waste
		DB.Order("created_at desc").Find(&wastes)
		return c.JSON(fiber.Map{"data": wastes})
	})

	log.Fatal(app.Listen(":3000"))
}