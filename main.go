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
    // Database
    DbHost = getEnv("DB_HOST", "localhost")

    // MinIO Config
    MinioEndpoint  = getEnv("MINIO_ENDPOINT", "minio-server:9000") // Internal Docker
    MinioPublicURL = getEnv("MINIO_PUBLIC_URL", "http://localhost:9000")

    MinioUser  = getEnv("MINIO_ACCESS_KEY", "admin")
    MinioPass  = getEnv("MINIO_SECRET_KEY", "password123")
    BucketName = getEnv("BUCKET_NAME", "waste-photos")

    // Lain-lain
    WaGatewayURL = "http://wa-gateway:3000"
    JWTSecret    = getEnv("JWT_SECRET", "rahasia-negara-maggot")
)

// --- DATABASE MODELS ---
type User struct {
    ID        uint      `gorm:"primaryKey" json:"id"`
    Email     string    `gorm:"unique" json:"email"`
    Password  string    `json:"-"`
    Role      string    `json:"role" gorm:"default:'user'"`
    CreatedAt time.Time `json:"created_at"`
}

type Waste struct {
    ID        uint      `gorm:"primaryKey" json:"id"`
    UserID    uint      `json:"user_id"`
    // 👇 FIX: Nambahin field User buat relasi Preload
    User      User      `gorm:"foreignKey:UserID" json:"user"` 
    Jenis     string    `json:"jenis" form:"jenis"`
    Berat     float64   `json:"berat" form:"berat"`
    FotoURL   string    `json:"foto_url"`
    Status    string    `json:"status" gorm:"default:'pending'"`
    CreatedAt time.Time `json:"created_at"`
}

var DB *gorm.DB
var MinioClient *minio.Client

func main() {
    // 1. KONEKSI DB
    databaseUrl := os.Getenv("DATABASE_URL")
    var dsn string
    if databaseUrl != "" {
        dsn = databaseUrl
        fmt.Println("☁️  Using Cloud Database")
    } else {
        dsn = fmt.Sprintf("host=%s user=postgres password=rahasia dbname=waste_db port=5432 sslmode=disable TimeZone=Asia/Jakarta", DbHost)
        fmt.Println("🏠 Using Database Host:", DbHost)
    }

    var err error
    DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
    if err != nil {
        log.Fatal("❌ Database Error:", err)
    }
    DB.AutoMigrate(&User{}, &Waste{})
    fmt.Println("✅ Sukses konek ke Database!")

    // 2. KONEKSI MINIO
    cleanEndpoint := strings.Replace(MinioEndpoint, "http://", "", 1)
    cleanEndpoint = strings.Replace(cleanEndpoint, "https://", "", 1)

    MinioClient, err = minio.New(cleanEndpoint, &minio.Options{
        Creds:  credentials.NewStaticV4(MinioUser, MinioPass, ""),
        Secure: false,
    })
    if err != nil {
        log.Println("⚠️ MinIO Error:", err)
    } else {
        fmt.Println("✅ Sukses konek ke MinIO Internal:", cleanEndpoint)
        
        // Auto Create Bucket
        ctx := context.Background()
        exists, errBucket := MinioClient.BucketExists(ctx, BucketName)
        if errBucket == nil && !exists {
            fmt.Println("⚙️ Bucket belum ada, mencoba membuat...", BucketName)
            MinioClient.MakeBucket(ctx, BucketName, minio.MakeBucketOptions{})
            
            // Set Public Policy
            policy := fmt.Sprintf(`{"Version": "2012-10-17","Statement": [{"Effect": "Allow","Principal": {"AWS": ["*"]},"Action": ["s3:GetObject"],"Resource": ["arn:aws:s3:::%s/*"]}]}`, BucketName)
            MinioClient.SetBucketPolicy(ctx, BucketName, policy)
            fmt.Println("🔓 Bucket berhasil dibuat & di-set PUBLIC!")
        }
    }

    // 3. SETUP SERVER
    app := fiber.New(fiber.Config{
        BodyLimit: 50 * 1024 * 1024,
    })

    app.Use(cors.New(cors.Config{
        AllowOrigins: "*", 
        AllowHeaders: "Origin, Content-Type, Accept, Authorization",
        AllowMethods: "GET, POST, PUT, DELETE",
    }))

    api := app.Group("/api")
    api.Post("/register", Register)
    api.Post("/login", Login)

    wasteRoutes := api.Group("/waste", AuthMiddleware)
    wasteRoutes.Post("/", CreateWaste)
    wasteRoutes.Get("/", GetWastes)
    wasteRoutes.Put("/:id/status", UpdateWasteStatus)
    wasteRoutes.Delete("/:id", DeleteWaste)

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

    filename := uuid.New().String() + filepath.Ext(file.Filename)
    fileBuffer, _ := file.Open()
    defer fileBuffer.Close()

    _, err = MinioClient.PutObject(context.Background(), BucketName, filename, fileBuffer, file.Size, minio.PutObjectOptions{
        ContentType: file.Header.Get("Content-Type"),
    })
    if err != nil {
        fmt.Println("❌ Gagal upload MinIO:", err)
        return c.Status(500).JSON(fiber.Map{"error": "Gagal upload ke Storage"})
    }

    // URL buat disimpan di DB
    finalURL := fmt.Sprintf("%s/%s/%s", strings.TrimRight(MinioPublicURL, "/"), BucketName, filename)
    
    waste.FotoURL = finalURL
    waste.UserID = uint(userID)
    DB.Create(&waste)

    // Notifikasi WhatsApp
    go func(fName string, msgJenis string, msgBerat float64, msgUser uint) {
        pesan := fmt.Sprintf("📢 *Laporan Baru Masuk!*\n\nJenis: %s\nBerat: %.2f Kg\nOleh User ID: %d", msgJenis, msgBerat, msgUser)
        SendWhatsApp("6289648186679", pesan, fName) 
    }(filename, waste.Jenis, waste.Berat, waste.UserID)

    return c.Status(201).JSON(fiber.Map{"message": "Laporan sukses & WA terkirim!", "data": waste})
}

func GetWastes(c *fiber.Ctx) error {
    var wastes []Waste
    // Pastiin baris ini ada Preload("User") biar Riwayat gak blank!
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

func SendWhatsApp(nomorTujuan string, pesan string, filename string) {
    if !strings.HasSuffix(nomorTujuan, "@s.whatsapp.net") {
        nomorTujuan = nomorTujuan + "@s.whatsapp.net"
    }

    targetURL := WaGatewayURL + "/send/message"
    
    if filename != "" {
        internalImgURL := fmt.Sprintf("http://%s/%s/%s", MinioEndpoint, BucketName, filename)
        respImg, err := http.Get(internalImgURL)
        if err != nil { return }
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
        if err == nil { io.Copy(part, respImg.Body) }
        writer.Close()

        targetURL = WaGatewayURL + "/send/image"
        http.Post(targetURL, writer.FormDataContentType(), body)
    } else {
        payload := map[string]interface{}{"phone": nomorTujuan, "message": pesan, "type": "text"}
        jsonPayload, _ := json.Marshal(payload)
        http.Post(targetURL, "application/json", bytes.NewBuffer(jsonPayload))
    }
}

func AuthMiddleware(c *fiber.Ctx) error {
    authHeader := c.Get("Authorization")
    if authHeader == "" { return c.Status(401).JSON(fiber.Map{"error": "Unauthorized"}) }
    tokenString := strings.Replace(authHeader, "Bearer ", "", 1)
    token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) { return []byte(JWTSecret), nil })
    if err != nil || !token.Valid { return c.Status(401).JSON(fiber.Map{"error": "Token Invalid"}) }
    claims := token.Claims.(jwt.MapClaims)
    c.Locals("user_id", claims["user_id"])
    c.Locals("role", claims["role"])
    return c.Next()
}