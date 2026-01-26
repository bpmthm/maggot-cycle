import urllib.request
import json
import urllib.error

# Daftar Alamat yang mau kita gedor
endpoints = [
    "/send", 
    "/api/send", 
    "/api/v1/send", 
    "/api/send-message", 
    "/send-message", 
    "/api/message/text", 
    "/api/v1/message/text", 
    "/api/message/send", 
    "/message/send", 
    "/chat/send"
]

base_url = "http://localhost:4000"
# Data dummy buat ngetes
data = json.dumps({"phone": "6285523568081", "message": "tes"}).encode('utf-8')
headers = {'Content-Type': 'application/json'}

print("=== 🕵️‍♂️ MULAI SCANNING VIA PYTHON ===")

for path in endpoints:
    url = base_url + path
    print(f"👉 Cek ke: {path} ...", end=" ")
    
    try:
        req = urllib.request.Request(url, data=data, headers=headers, method='POST')
        with urllib.request.urlopen(req) as f:
            response = f.read().decode('utf-8')
            print("✅ KETEMU!")
            print(f"🎉 RESPONSE SERVER: {response}")
            print(f"🔥 URL YANG BENER ADALAH: {url}")
            break
            
    except urllib.error.HTTPError as e:
        # Kalau errornya 404, berarti alamat salah.
        if e.code == 404:
            print("❌ Gak Ada (404)")
        # Kalau errornya 400/500/405, BERARTI ALAMATNYA BENER, cuma datanya aja yang kurang pas
        else:
            print(f"⚠️ MENCURIGAKAN ({e.code})!")
            print(f"💡 Kemungkinan ini URL-nya bener, tapi server nolak format datanya.")
            print(f"🔥 COBA URL INI: {url}")
            break
            
    except Exception as e:
        print(f"Error lain: {e}")

print("=== SELESAI ===")