from flask import Flask
import numpy as np

app = Flask(__name__)

@app.route("/")
def predict():
    data = np.array([[1.0, 2.0, 3.0]])
    return f"Prediction shape: {data.shape}"

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8080)
