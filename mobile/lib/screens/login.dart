import 'package:flutter/material.dart';

import '../api.dart';
import '../storage.dart';
import 'files.dart';

/// Вход: адрес сервера, имя пользователя и пароль приложения (не основной пароль).
class LoginScreen extends StatefulWidget {
  const LoginScreen({super.key, this.connect});

  /// Для тестов: подменить проверку подключения.
  final Future<CloudBoxApi> Function(String server, String user, String password)? connect;

  @override
  State<LoginScreen> createState() => _LoginScreenState();
}

class _LoginScreenState extends State<LoginScreen> {
  final _server = TextEditingController();
  final _user = TextEditingController();
  final _pass = TextEditingController();
  final _form = GlobalKey<FormState>();
  String? _error;
  bool _busy = false;

  Future<CloudBoxApi> _defaultConnect(String s, String u, String p) async {
    final api = CloudBoxApi(s, u, p);
    await api.me(); // проверка адреса и пароля
    return api;
  }

  Future<void> _submit() async {
    if (!_form.currentState!.validate()) return;
    setState(() {
      _busy = true;
      _error = null;
    });
    try {
      final api = await (widget.connect ?? _defaultConnect)(_server.text, _user.text.trim(), _pass.text.trim());
      await Storage.save(Account(api.base.toString(), api.user, _pass.text.trim()));
      if (!mounted) return;
      Navigator.of(context).pushReplacement(MaterialPageRoute(builder: (_) => FilesScreen(api: api)));
    } on ApiException catch (e) {
      setState(() => _error = e.message);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final insecure = _server.text.trim().startsWith('http://');
    return Scaffold(
      body: SafeArea(
        child: Center(
          child: SingleChildScrollView(
            padding: const EdgeInsets.all(24),
            child: ConstrainedBox(
              constraints: const BoxConstraints(maxWidth: 420),
              child: Form(
                key: _form,
                child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
                  const Icon(Icons.cloud, size: 64, color: Color(0xFF2F6FED)),
                  const SizedBox(height: 8),
                  Text('CloudBox', textAlign: TextAlign.center, style: Theme.of(context).textTheme.headlineMedium),
                  const SizedBox(height: 24),
                  TextFormField(
                    key: const Key('server'),
                    controller: _server,
                    keyboardType: TextInputType.url,
                    autocorrect: false,
                    decoration: const InputDecoration(labelText: 'Адрес сервера', hintText: 'cloud.example.ru'),
                    onChanged: (_) => setState(() {}),
                    validator: (v) => (v ?? '').trim().isEmpty ? 'Укажите адрес сервера' : null,
                  ),
                  if (insecure)
                    const Padding(
                      padding: EdgeInsets.only(top: 6),
                      child: Text('⚠️ Без HTTPS пароль идёт по сети открытым текстом. Используйте http:// только в домашней сети.',
                          style: TextStyle(color: Colors.orange)),
                    ),
                  TextFormField(
                    key: const Key('user'),
                    controller: _user,
                    autocorrect: false,
                    decoration: const InputDecoration(labelText: 'Имя пользователя'),
                    validator: (v) => (v ?? '').trim().isEmpty ? 'Укажите имя пользователя' : null,
                  ),
                  TextFormField(
                    key: const Key('password'),
                    controller: _pass,
                    obscureText: true,
                    decoration: const InputDecoration(labelText: 'Пароль приложения'),
                    validator: (v) => (v ?? '').trim().isEmpty ? 'Укажите пароль приложения' : null,
                    onFieldSubmitted: (_) => _submit(),
                  ),
                  const SizedBox(height: 6),
                  Text('Создайте пароль приложения на сайте CloudBox: Настройки → Пароли приложений. '
                      'Он работает и с двухфакторной аутентификацией, и его можно отозвать в любой момент.',
                      style: Theme.of(context).textTheme.bodySmall),
                  if (_error != null)
                    Padding(
                      padding: const EdgeInsets.only(top: 12),
                      child: Text(_error!, key: const Key('error'), style: TextStyle(color: Theme.of(context).colorScheme.error)),
                    ),
                  const SizedBox(height: 20),
                  FilledButton(
                    key: const Key('login'),
                    onPressed: _busy ? null : _submit,
                    child: _busy ? const SizedBox(height: 20, width: 20, child: CircularProgressIndicator(strokeWidth: 2)) : const Text('Войти'),
                  ),
                ]),
              ),
            ),
          ),
        ),
      ),
    );
  }
}
