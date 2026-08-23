package utils

import (
	"io"
	"log"
	"os"

	"github.com/sirupsen/logrus"
)

// Logger é o logger centralizado do projeto
var Logger *logrus.Logger

func init() {
	Logger = logrus.New()

	// Configuração básica de logging
	Logger.SetLevel(logrus.InfoLevel)
	Logger.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: "2006-01-02 15:04:05",
	})
	Logger.SetOutput(os.Stdout)

	// Desabilita a saída padrão do Go (log package)
	log.SetOutput(io.Discard)
}

// Debug logs uma mensagem de debug
func Debug(msg string) {
	Logger.Debug(msg)
}

// Info logs uma mensagem de info
func Info(msg string) {
	Logger.Info(msg)
}

// Warn logs uma mensagem de aviso
func Warn(msg string) {
	Logger.Warn(msg)
}

// Warnf logs uma mensagem de aviso formatada
func Warnf(format string, args ...interface{}) {
	Logger.Warnf(format, args...)
}

// Error logs uma mensagem de erro
func Error(msg string) {
	Logger.Error(msg)
}

// Fatal logs uma mensagem de erro fatal e encerra o processo
func Fatal(msg string) {
	Logger.Fatal(msg)
}

// Infof logs uma mensagem de info formatada
func Infof(format string, args ...interface{}) {
	Logger.Infof(format, args...)
}

// Errorf logs uma mensagem de erro formatada
func Errorf(format string, args ...interface{}) {
	Logger.Errorf(format, args...)
}
