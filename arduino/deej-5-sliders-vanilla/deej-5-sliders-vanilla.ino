const int NUM_SLIDERS = 7;
const int analogInputs[NUM_SLIDERS] = {A0, A1, A2, A3, A4, A5, A6};
const int NUM_BUTTONS = 5;
const int buttonInputs[NUM_BUTTONS] = {2, 3, 4, 5, 6};
const unsigned long BUTTON_DEBOUNCE_MS = 25;

int analogSliderValues[NUM_SLIDERS];
int buttonReadings[NUM_BUTTONS];
int buttonStates[NUM_BUTTONS];
unsigned long buttonChangedAt[NUM_BUTTONS];

void setup() { 
  for (int i = 0; i < NUM_SLIDERS; i++) {
    pinMode(analogInputs[i], INPUT);
  }

  for (int i = 0; i < NUM_BUTTONS; i++) {
    pinMode(buttonInputs[i], INPUT_PULLUP);
    buttonReadings[i] = digitalRead(buttonInputs[i]);
    buttonStates[i] = buttonReadings[i] == LOW;
    buttonChangedAt[i] = millis();
  }

  Serial.begin(9600);
}

void loop() {
  updateSliderValues();
  updateButtonStates();
  sendSliderValues(); // Actually send data (all the time)
  // printSliderValues(); // For debug
  delay(10);
}

void updateButtonStates() {
  for (int i = 0; i < NUM_BUTTONS; i++) {
    int reading = digitalRead(buttonInputs[i]);
    if (reading != buttonReadings[i]) {
      buttonReadings[i] = reading;
      buttonChangedAt[i] = millis();
    }

    if (millis() - buttonChangedAt[i] >= BUTTON_DEBOUNCE_MS) {
      buttonStates[i] = reading == LOW;
    }
  }
}

void updateSliderValues() {
  for (int i = 0; i < NUM_SLIDERS; i++) {
     analogSliderValues[i] = analogRead(analogInputs[i]);
  }
}

void sendSliderValues() {
  String builtString = String("");

  for (int i = 0; i < NUM_SLIDERS; i++) {
    builtString += String((int)analogSliderValues[i]);

    if (i < NUM_SLIDERS - 1) {
      builtString += String("|");
    }
  }

  builtString += String(";");
  for (int i = 0; i < NUM_BUTTONS; i++) {
    builtString += String(buttonStates[i] ? "1" : "0");

    if (i < NUM_BUTTONS - 1) {
      builtString += String("|");
    }
  }
  
  Serial.println(builtString);
}

void printSliderValues() {
  for (int i = 0; i < NUM_SLIDERS; i++) {
    String printedString = String("Slider #") + String(i + 1) + String(": ") + String(analogSliderValues[i]) + String(" mV");
    Serial.write(printedString.c_str());

    if (i < NUM_SLIDERS - 1) {
      Serial.write(" | ");
    } else {
      Serial.write("\n");
    }
  }
}
