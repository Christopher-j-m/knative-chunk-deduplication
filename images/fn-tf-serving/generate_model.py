"""Saves a tiny Keras model next to this script for the TensorFlow Serving image to ship"""
import os

import tensorflow as tf

output_dir = os.path.join(os.path.dirname(os.path.abspath(__file__)), "saved_model")
model = tf.keras.Sequential([tf.keras.Input(shape=(1,)), tf.keras.layers.Dense(1)])
model.export(output_dir)
