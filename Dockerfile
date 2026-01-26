FROM golang:alpine

WORKDIR /app

# Copy daftar belanjaan (library) dulu biar cache-nya awet
COPY go.mod go.sum ./
RUN go mod download

# Copy semua kodingan ke dalam container
COPY . .

# Jalanin aplikasinya
CMD ["go", "run", "main.go"]