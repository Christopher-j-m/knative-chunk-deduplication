import tensorflow as tf
model = tf.keras.Sequential([tf.keras.layers.Dense(1, input_shape=(1,))])
tf.saved_model.save(model, "images/fn-tf-serving/saved_model")
