import requests
import json

url = "https://api.pjjrn.my.id/api/login"
headers = {"Content-Type": "application/json"}

# Pake akun yang barusan kita bikin lewat Curl
payload = {
    "email": "boss@maggot.com",
    "password": "admin123"
}

print(f"🚀 Login pake: {payload['email']}")
try:
    response = requests.post(url, json=payload, headers=headers)
    print(f"📡 Status: {response.status_code}")
    if response.status_code == 200:
        print("✅ TOKEN DAPET!:", response.json().get('token')[:30], "...")
    else:
        print("❌ Gagal:", response.text)
except Exception as e:
    print("Error:", e)
