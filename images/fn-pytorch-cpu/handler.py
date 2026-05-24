import torch
from flask import Flask, jsonify

app = Flask(__name__)
model = torch.nn.Linear(4, 1)
model.eval()

@app.route("/")
def predict():
    x = torch.randn(1, 4)
    with torch.no_grad():
        y = model(x)
    return jsonify({"result": y.item()})

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8080)
