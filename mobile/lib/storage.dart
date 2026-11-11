// Хранение входа и настроек. Пароль приложения — в защищённом хранилище ОС
// (Keychain на iOS, Keystore на Android), остальное — в обычных настройках.
import 'package:flutter_secure_storage/flutter_secure_storage.dart';
import 'package:shared_preferences/shared_preferences.dart';

class Account {
  final String server;
  final String user;
  final String password;
  Account(this.server, this.user, this.password);
}

class Storage {
  // Android: ключи в Keystore, данные — AES-GCM (по умолчанию в flutter_secure_storage 11); iOS: Keychain
  static const _secure = FlutterSecureStorage();

  static Future<Account?> load() async {
    final server = await _secure.read(key: 'server');
    final user = await _secure.read(key: 'user');
    final pass = await _secure.read(key: 'password');
    if (server == null || user == null || pass == null) return null;
    return Account(server, user, pass);
  }

  static Future<void> save(Account a) async {
    await _secure.write(key: 'server', value: a.server);
    await _secure.write(key: 'user', value: a.user);
    await _secure.write(key: 'password', value: a.password);
  }

  /// Выход: забываем пароль и состояние автозагрузки (у другого аккаунта своя папка).
  static Future<void> clear() async {
    await _secure.deleteAll();
    final p = await SharedPreferences.getInstance();
    await p.remove(autoUploadKey);
    await p.remove(autoUploadSinceKey);
    await p.remove(autoUploadDoneKey);
    await p.remove(autoUploadLastRunKey);
  }

  static const autoUploadKey = 'autoupload_enabled';
  static const autoUploadSinceKey = 'autoupload_since_ms';
  static const autoUploadDoneKey = 'autoupload_done';
  static const autoUploadLastRunKey = 'autoupload_last_run_ms';
  static const wifiOnlyKey = 'autoupload_wifi_only';
  static const chargingOnlyKey = 'autoupload_charging_only';
}
